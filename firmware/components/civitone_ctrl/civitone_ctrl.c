// Civitone node control channel. See docs/control-protocol.md.
//
// The node always dials out: one WebSocket to the controller, a hello on every
// connect, a heartbeat every 5 s, and an ack for every command. Audio stays on
// the Snapcast stream; volume and routing are owned by snapserver.
//
// Adoption (docs/node-setup.md): the setup page saves the controller URL and
// an adopt code made in the console. Unadopted, the node says hello with that
// code; the controller answers "welcome" (pending) and, once an operator
// approves, sends "adopt" with a device token and the snapserver address.
// From then on the node sends "Authorization: Bearer <token>" and no code.
// Close code 4003 (bad code, rejected) or 4001 (unknown token) clears the
// credential, and the node goes back to setup.
//
// With CONFIG_CIVITONE_ROLE_SOURCE the same channel serves an input board: the
// hello says role "source", and the board's capture task pushes its audio as
// binary frames (civitone_ctrl_send_audio) once the controller has adopted it.

#include "civitone_ctrl.h"

#include <stdio.h>
#include <string.h>
#include <strings.h>

#include "cJSON.h"
#include "esp_crt_bundle.h"
#include "esp_http_client.h"
#include "esp_log.h"
#include "esp_mac.h"
#include "esp_netif.h"
#include "esp_ota_ops.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "esp_websocket_client.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "mbedtls/sha256.h"
#include "nvs.h"
#include "settings_manager.h"

static const char *TAG = "CIVI_CTRL";

#define CIVI_FW_VERSION "0.4.2"
#define CIVI_BOARD CONFIG_CIVITONE_BOARD
#ifdef CONFIG_CIVITONE_ROLE_SOURCE
#define CIVI_ID_PREFIX "src"
#else
#define CIVI_ID_PREFIX "civ"
#endif
#define CIVI_NVS_NS "civitone"
#define CIVI_HB_MS 5000
#define CIVI_URL_MAX 160
#define CIVI_TOKEN_MAX 72
#define CIVI_CODE_MAX 16
#define CIVI_CLOSE_TOKEN_REJECTED 4001
#define CIVI_CLOSE_CODE_REJECTED 4003

// Underrun counter from patch 0004 (player.c). Weak so this component links
// even without that patch.
uint32_t civi_player_underruns(void) __attribute__((weak));

// Board-specific commands and heartbeat fields (civitone_ctrl.h). Weak:
// absent means none.
bool civitone_board_command(const char *cmd, const cJSON *msg, const char **err) __attribute__((weak));
void civitone_board_heartbeat(cJSON *hb) __attribute__((weak));

// Progress for the setup page (civitone_setup). Weak so this component
// doesn't depend on it; the values match civi_srv_state_t.
void civitone_setup_note_server(int state, const char *detail) __attribute__((weak));
enum { SRV_IDLE, SRV_CONNECTING, SRV_PENDING, SRV_ADOPTED, SRV_REJECTED, SRV_ERROR };

static void note(int state, const char *detail) {
  if (civitone_setup_note_server) civitone_setup_note_server(state, detail);
}

static esp_websocket_client_handle_t s_client;
// Held while the client is built or torn down, and by senders on other tasks
// (audio), so they never touch a destroyed client. Not taken on the client's
// own task: client_stop waits for that task.
static SemaphoreHandle_t s_client_lock;
static volatile bool s_live = false;  // connected and welcomed as adopted
static volatile uint32_t s_session = 0;  // counts adopted welcomes
static char s_device_id[16];
static char s_mac[18];
static char s_token[CIVI_TOKEN_MAX];
static char s_code[CIVI_CODE_MAX];
static char s_auth_header[CIVI_TOKEN_MAX + 32];
static volatile int s_volume = -1;
static volatile bool s_muted = false;
static volatile bool s_restart_pending = false;
static volatile bool s_reload = false;   // settings changed: rebuild the client
static volatile bool s_parked = false;   // rejected: stop dialling until reload
static volatile bool s_hb_now = false;   // another task wants a heartbeat out now

static inline void send_heartbeat_soon(void) { s_hb_now = true; }

void civitone_ctrl_note_dac(int volume, bool muted) {
  s_volume = volume;
  s_muted = muted;
}

#ifndef CONFIG_CIVITONE_ROLE_SOURCE
// Speakers: the left and right outputs each drive one speaker on the pole (A
// and B). Each has a trim below the zone volume and a mute, kept in NVS and
// applied in the DAC (patch 0010). Audio is mono, so both play the same
// thing.
#define CIVI_TRIM_MIN_HALF_DB (-48)  // -24 dB
typedef struct {
  int8_t trim_half_db;  // 0 .. CIVI_TRIM_MIN_HALF_DB, in 0.5 dB steps
  uint8_t muted;
} civi_spk_t;
static civi_spk_t s_spk[2];

esp_err_t pcm51xx_reapply_volume(void) __attribute__((weak));
static void add_ota(cJSON *hb);  // firmware update progress, below

// Called by the DAC driver for every volume write.
int civitone_ctrl_speaker_steps(int ch) {
  if (ch < 0 || ch > 1) return 0;
  return s_spk[ch].muted ? -1 : -s_spk[ch].trim_half_db;
}

static void load_speakers(void) {
  nvs_handle_t h;
  if (nvs_open(CIVI_NVS_NS, NVS_READONLY, &h) != ESP_OK) return;
  civi_spk_t v[2];
  size_t n = sizeof v;
  if (nvs_get_blob(h, "speakers", v, &n) == ESP_OK && n == sizeof v) memcpy(s_spk, v, sizeof v);
  nvs_close(h);
}

static esp_err_t save_speakers(void) {
  nvs_handle_t h;
  esp_err_t err = nvs_open(CIVI_NVS_NS, NVS_READWRITE, &h);
  if (err != ESP_OK) return err;
  err = nvs_set_blob(h, "speakers", s_spk, sizeof s_spk);
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

// {"a":{"trim_db":-6,"muted":false},"b":{...}}: either speaker, either
// field; what's left out stays as it is. "save":false applies without
// writing NVS (the controller's chirp mutes one speaker for a moment).
static const char *set_speakers(const cJSON *msg) {
  civi_spk_t next[2];
  memcpy(next, s_spk, sizeof next);
  const char *names[2] = {"a", "b"};
  for (int ch = 0; ch < 2; ch++) {
    const cJSON *s = cJSON_GetObjectItem(msg, names[ch]);
    if (!s) continue;
    if (!cJSON_IsObject(s)) return "speakers must be objects";
    const cJSON *trim = cJSON_GetObjectItem(s, "trim_db");
    if (trim) {
      if (!cJSON_IsNumber(trim) || trim->valuedouble > 0 || trim->valuedouble < CIVI_TRIM_MIN_HALF_DB / 2.0)
        return "trim_db must be -24..0";
      next[ch].trim_half_db = (int8_t)(trim->valuedouble * 2 - 0.5);  // round to the nearest 0.5 dB
    }
    const cJSON *muted = cJSON_GetObjectItem(s, "muted");
    if (muted) {
      if (!cJSON_IsBool(muted)) return "muted must be true or false";
      next[ch].muted = cJSON_IsTrue(muted);
    }
  }
  memcpy(s_spk, next, sizeof s_spk);
  if (!cJSON_IsFalse(cJSON_GetObjectItem(msg, "save")) && save_speakers() != ESP_OK)
    ESP_LOGE(TAG, "saving speakers failed; applied until reboot");
  if (pcm51xx_reapply_volume) pcm51xx_reapply_volume();
  ESP_LOGI(TAG, "speakers: A %.1f dB%s, B %.1f dB%s", s_spk[0].trim_half_db / 2.0,
           s_spk[0].muted ? " muted" : "", s_spk[1].trim_half_db / 2.0, s_spk[1].muted ? " muted" : "");
  return NULL;
}

static void add_speakers(cJSON *hb) {
  cJSON *o = cJSON_AddObjectToObject(hb, "speakers");
  const char *names[2] = {"a", "b"};
  for (int ch = 0; ch < 2; ch++) {
    cJSON *s = cJSON_AddObjectToObject(o, names[ch]);
    cJSON_AddNumberToObject(s, "trim_db", s_spk[ch].trim_half_db / 2.0);
    cJSON_AddBoolToObject(s, "muted", s_spk[ch].muted);
  }
}
#endif

void civitone_ctrl_reload(void) {
  s_parked = false;
  s_reload = true;
}

static void load_identity(void) {
  uint8_t mac[6];
  esp_read_mac(mac, ESP_MAC_WIFI_STA);
  snprintf(s_mac, sizeof s_mac, "%02X:%02X:%02X:%02X:%02X:%02X", mac[0],
           mac[1], mac[2], mac[3], mac[4], mac[5]);
  snprintf(s_device_id, sizeof s_device_id, CIVI_ID_PREFIX "-%02x%02x", mac[4], mac[5]);
}

const char *civitone_ctrl_device_id(void) {
  if (!s_device_id[0]) load_identity();
  return s_device_id;
}

uint32_t civitone_ctrl_session(void) { return s_live ? s_session : 0; }

const char *civitone_ctrl_fw_version(void) { return CIVI_FW_VERSION; }

static char s_name[48];  // from the controller's welcome, kept in NVS

const char *civitone_ctrl_name(void) { return s_name; }

bool civitone_ctrl_send_text(const char *json) {
  if (!s_live || !s_client_lock) return false;
  if (xSemaphoreTake(s_client_lock, pdMS_TO_TICKS(100)) != pdTRUE) return false;
  bool ok = s_client && s_live && esp_websocket_client_is_connected(s_client) &&
            esp_websocket_client_send_text(s_client, json, strlen(json), pdMS_TO_TICKS(500)) >= 0;
  xSemaphoreGive(s_client_lock);
  return ok;
}

bool civitone_ctrl_send_audio(const void *pcm, size_t len) {
  if (!s_live || !s_client_lock) return false;
  if (xSemaphoreTake(s_client_lock, 0) != pdTRUE) return false;
  bool ok = s_client && s_live && esp_websocket_client_is_connected(s_client) &&
            esp_websocket_client_send_bin(s_client, pcm, len, pdMS_TO_TICKS(200)) == (int)len;
  xSemaphoreGive(s_client_lock);
  return ok;
}

static bool nvs_read(const char *key, char *out, size_t len) {
  nvs_handle_t h;
  out[0] = 0;
  if (nvs_open(CIVI_NVS_NS, NVS_READONLY, &h) != ESP_OK) return false;
  size_t n = len;
  esp_err_t err = nvs_get_str(h, key, out, &n);
  nvs_close(h);
  if (err != ESP_OK) out[0] = 0;
  return err == ESP_OK && out[0];
}

static esp_err_t nvs_write(const char *key, const char *val) {
  nvs_handle_t h;
  esp_err_t err = nvs_open(CIVI_NVS_NS, NVS_READWRITE, &h);
  if (err != ESP_OK) return err;
  err = val ? nvs_set_str(h, key, val) : nvs_erase_key(h, key);
  if (err == ESP_ERR_NVS_NOT_FOUND) err = ESP_OK;
  if (err == ESP_OK) err = nvs_commit(h);
  nvs_close(h);
  return err;
}

// controller_url from NVS, else derived from the snapserver host (nodes set
// up before setup mode existed).
static bool controller_url(char *out, size_t len) {
  if (nvs_read("ctrl_url", out, len)) return true;
  char host[64] = {0};
  if (settings_get_server_host(host, sizeof host) == ESP_OK && host[0]) {
    snprintf(out, len, "ws://%s:8080/node", host);
    return true;
  }
  return false;
}

static void send_json(cJSON *obj) {
  char *s = cJSON_PrintUnformatted(obj);
  if (s && s_client && esp_websocket_client_is_connected(s_client)) {
    esp_websocket_client_send_text(s_client, s, strlen(s), pdMS_TO_TICKS(2000));
  }
  cJSON_free(s);
}

static void send_hello(void) {
  cJSON *o = cJSON_CreateObject();
  cJSON_AddStringToObject(o, "t", "hello");
  cJSON_AddStringToObject(o, "device_id", s_device_id);
  cJSON_AddStringToObject(o, "mac", s_mac);
  cJSON_AddStringToObject(o, "fw", CIVI_FW_VERSION);
  cJSON_AddStringToObject(o, "board", CIVI_BOARD);
#ifdef CONFIG_CIVITONE_ROLE_SOURCE
  cJSON_AddStringToObject(o, "role", "source");
  cJSON_AddStringToObject(o, "kind", "board");
  cJSON_AddNumberToObject(o, "rate", 48000);
  cJSON_AddNumberToObject(o, "channels", 1);
#endif
  if (!s_token[0] && s_code[0]) cJSON_AddStringToObject(o, "adopt_code", s_code);
  send_json(o);
  cJSON_Delete(o);
}

static void send_heartbeat(void) {
  cJSON *o = cJSON_CreateObject();
  cJSON_AddStringToObject(o, "t", "hb");

  wifi_ap_record_t ap;
  if (esp_wifi_sta_get_ap_info(&ap) == ESP_OK) {
    cJSON_AddNumberToObject(o, "rssi", ap.rssi);
    cJSON_AddStringToObject(o, "ssid", (const char *)ap.ssid);
  }
  esp_netif_t *netif = esp_netif_get_handle_from_ifkey("WIFI_STA_DEF");
  esp_netif_ip_info_t ip;
  if (netif && esp_netif_get_ip_info(netif, &ip) == ESP_OK) {
    char buf[16];
    snprintf(buf, sizeof buf, IPSTR, IP2STR(&ip.ip));
    cJSON_AddStringToObject(o, "ip", buf);
  }
  if (s_volume >= 0) {
    cJSON_AddNumberToObject(o, "volume", s_volume);
    cJSON_AddBoolToObject(o, "muted", s_muted);
  }
  if (civi_player_underruns) {
    cJSON_AddNumberToObject(o, "underruns", civi_player_underruns());
  }
  cJSON_AddNumberToObject(o, "uptime", esp_timer_get_time() / 1000000);
  cJSON_AddNumberToObject(o, "heap", esp_get_free_heap_size());
#ifndef CONFIG_CIVITONE_ROLE_SOURCE
  add_speakers(o);
  add_ota(o);
#endif
  if (civitone_board_heartbeat) civitone_board_heartbeat(o);
  send_json(o);
  cJSON_Delete(o);
}

static void send_ack(double id, const char *err) {
  cJSON *o = cJSON_CreateObject();
  cJSON_AddStringToObject(o, "t", "ack");
  cJSON_AddNumberToObject(o, "id", id);
  cJSON_AddBoolToObject(o, "ok", err == NULL);
  if (err) cJSON_AddStringToObject(o, "err", err);
  send_json(o);
  cJSON_Delete(o);
}

static void restart_task(void *arg) {
  vTaskDelay(pdMS_TO_TICKS(500));  // let the ack go out
  esp_restart();
}

static void restart_soon(void) {
  if (!s_restart_pending) {
    s_restart_pending = true;
    xTaskCreate(restart_task, "civi_restart", 2048, NULL, 5, NULL);
  }
}

#ifndef CONFIG_CIVITONE_ROLE_SOURCE
// Firmware updates (update_firmware). The node fetches the image from the
// controller it already talks to, with its device token, writes the spare
// OTA slot, checks the SHA-256 the controller sent, and reboots into it. The
// new image stays on probation (rollback enabled) until it reconnects and is
// welcomed as adopted; a reboot before that goes back to the old one.
typedef enum { OTA_IDLE, OTA_DOWNLOADING, OTA_REBOOTING, OTA_FAILED } civi_ota_state_t;
static const char *const OTA_STATE_NAMES[] = {"idle", "downloading", "rebooting", "failed"};
static volatile civi_ota_state_t s_ota_state = OTA_IDLE;
static volatile int s_ota_pct = 0;
static char s_ota_err[64];

typedef struct {
  char url[CIVI_URL_MAX + 96];
  char sha256[65];
  int size;
} civi_ota_job_t;

static void ota_fail(const char *why) {
  snprintf(s_ota_err, sizeof s_ota_err, "%s", why);
  s_ota_state = OTA_FAILED;
  ESP_LOGE(TAG, "firmware update failed: %s", why);
}

static void ota_task(void *arg) {
  civi_ota_job_t *j = arg;
  const esp_partition_t *part = esp_ota_get_next_update_partition(NULL);
  esp_http_client_handle_t c = NULL;
  esp_ota_handle_t ota = 0;
  bool ota_open = false;
  uint8_t *buf = malloc(4096);
  mbedtls_sha256_context sha;
  mbedtls_sha256_init(&sha);
  mbedtls_sha256_starts(&sha, 0);
  char why[64] = "";

  if (!part || !buf) {
    snprintf(why, sizeof why, "no OTA slot or no memory");
    goto done;
  }
  esp_http_client_config_t cfg = {
      .url = j->url,
      .crt_bundle_attach = esp_crt_bundle_attach,
      .timeout_ms = 15000,
  };
  c = esp_http_client_init(&cfg);
  char bearer[CIVI_TOKEN_MAX + 8];
  snprintf(bearer, sizeof bearer, "Bearer %s", s_token);
  esp_http_client_set_header(c, "Authorization", bearer);
  esp_http_client_set_header(c, "X-Civitone-Device", s_device_id);
  if (esp_http_client_open(c, 0) != ESP_OK) {
    snprintf(why, sizeof why, "can't reach the controller");
    goto done;
  }
  int len = esp_http_client_fetch_headers(c);
  int status = esp_http_client_get_status_code(c);
  if (status != 200) {
    snprintf(why, sizeof why, "controller answered HTTP %d", status);
    goto done;
  }
  if (len <= 0 || (j->size > 0 && len != j->size)) {
    snprintf(why, sizeof why, "unexpected size %d", len);
    goto done;
  }
  if (len > (int)part->size) {
    snprintf(why, sizeof why, "image %d bytes, slot %lu", len, (unsigned long)part->size);
    goto done;
  }
  if (esp_ota_begin(part, OTA_WITH_SEQUENTIAL_WRITES, &ota) != ESP_OK) {
    snprintf(why, sizeof why, "can't start writing the OTA slot");
    goto done;
  }
  ota_open = true;
  int got = 0;
  for (;;) {
    int n = esp_http_client_read(c, (char *)buf, 4096);
    if (n < 0) {
      snprintf(why, sizeof why, "download broke off at %d bytes", got);
      goto done;
    }
    if (n == 0) break;
    if (esp_ota_write(ota, buf, n) != ESP_OK) {
      snprintf(why, sizeof why, "flash write failed");
      goto done;
    }
    mbedtls_sha256_update(&sha, buf, n);
    got += n;
    s_ota_pct = (int)((int64_t)got * 100 / len);
  }
  if (got != len) {
    snprintf(why, sizeof why, "got %d of %d bytes", got, len);
    goto done;
  }
  uint8_t digest[32];
  char hex[65];
  mbedtls_sha256_finish(&sha, digest);
  for (int i = 0; i < 32; i++) snprintf(hex + i * 2, 3, "%02x", digest[i]);
  if (strcasecmp(hex, j->sha256) != 0) {
    snprintf(why, sizeof why, "checksum mismatch");
    goto done;
  }
  ota_open = false;
  if (esp_ota_end(ota) != ESP_OK) {  // also checks it's a valid app image
    snprintf(why, sizeof why, "not a valid firmware image");
    goto done;
  }
  if (esp_ota_set_boot_partition(part) != ESP_OK) {
    snprintf(why, sizeof why, "can't switch the boot slot");
    goto done;
  }
  s_ota_state = OTA_REBOOTING;
  ESP_LOGW(TAG, "firmware update written (%d bytes), rebooting into it", got);

done:
  mbedtls_sha256_free(&sha);
  if (ota_open) esp_ota_abort(ota);
  if (c) esp_http_client_cleanup(c);
  free(buf);
  free(j);
  if (s_ota_state == OTA_REBOOTING) {
    send_heartbeat_soon();
    vTaskDelay(pdMS_TO_TICKS(1500));  // let the heartbeat say so
    esp_restart();
  }
  ota_fail(why[0] ? why : "unknown error");
  vTaskDelete(NULL);
}

// {"path":"/firmware/<sha256>.bin","sha256":"...","size":N}, or a full "url".
// A path is fetched from the controller this node is connected to.
static const char *update_firmware(const cJSON *msg) {
  if (s_ota_state == OTA_DOWNLOADING || s_ota_state == OTA_REBOOTING) return "an update is already running";
  const char *sha = cJSON_GetStringValue(cJSON_GetObjectItem(msg, "sha256"));
  const char *url = cJSON_GetStringValue(cJSON_GetObjectItem(msg, "url"));
  const char *path = cJSON_GetStringValue(cJSON_GetObjectItem(msg, "path"));
  const cJSON *size = cJSON_GetObjectItem(msg, "size");
  if (!sha || strlen(sha) != 64) return "sha256 required";
  if (!url && (!path || path[0] != '/')) return "path or url required";
  civi_ota_job_t *j = calloc(1, sizeof *j);
  if (!j) return "out of memory";
  if (url) {
    snprintf(j->url, sizeof j->url, "%s", url);
  } else {
    // ws://host[:port]/node -> http://host[:port], wss:// -> https://
    char ctrl[CIVI_URL_MAX];
    if (!controller_url(ctrl, sizeof ctrl) || strncmp(ctrl, "ws", 2) != 0) {
      free(j);
      return "no controller URL";
    }
    char *host = strstr(ctrl, "://");
    char *slash = host ? strchr(host + 3, '/') : NULL;
    if (slash) *slash = 0;
    snprintf(j->url, sizeof j->url, "http%s%s", ctrl + 2, path);
  }
  snprintf(j->sha256, sizeof j->sha256, "%s", sha);
  j->size = cJSON_IsNumber(size) ? size->valueint : 0;
  s_ota_err[0] = 0;
  s_ota_pct = 0;
  s_ota_state = OTA_DOWNLOADING;
  ESP_LOGW(TAG, "firmware update from %s", j->url);
  if (xTaskCreate(ota_task, "civi_ota", 8192, j, 3, NULL) != pdPASS) {
    free(j);
    ota_fail("can't start the update task");
  }
  return NULL;
}

static void add_ota(cJSON *hb) {
  const esp_partition_t *run = esp_ota_get_running_partition();
  esp_ota_img_states_t st;
  if (run && esp_ota_get_state_partition(run, &st) == ESP_OK && st == ESP_OTA_IMG_PENDING_VERIFY)
    cJSON_AddBoolToObject(hb, "fw_probation", true);
  if (s_ota_state == OTA_IDLE) return;
  cJSON *o = cJSON_AddObjectToObject(hb, "ota");
  cJSON_AddStringToObject(o, "state", OTA_STATE_NAMES[s_ota_state]);
  if (s_ota_state == OTA_DOWNLOADING) cJSON_AddNumberToObject(o, "pct", s_ota_pct);
  if (s_ota_state == OTA_FAILED) cJSON_AddStringToObject(o, "err", s_ota_err);
}

// The running image is new and on probation: it reached the controller and
// was welcomed, so keep it.
static void confirm_firmware(void) {
  const esp_partition_t *run = esp_ota_get_running_partition();
  esp_ota_img_states_t st;
  if (run && esp_ota_get_state_partition(run, &st) == ESP_OK && st == ESP_OTA_IMG_PENDING_VERIFY) {
    esp_ota_mark_app_valid_cancel_rollback();
    ESP_LOGW(TAG, "new firmware %s checked in with the controller: keeping it", CIVI_FW_VERSION);
  }
}
#endif

// Returns NULL on success or an error string for the ack.
static const char *run_command(const cJSON *msg) {
  const char *cmd = cJSON_GetStringValue(cJSON_GetObjectItem(msg, "cmd"));
  if (!cmd) return "missing cmd";

  if (strcmp(cmd, "identify") == 0) {
    const cJSON *secs = cJSON_GetObjectItem(msg, "seconds");
    ESP_LOGW(TAG, "IDENTIFY for %d s", cJSON_IsNumber(secs) ? secs->valueint : 5);
    return NULL;
  }
  if (strcmp(cmd, "reboot") == 0) {
    ESP_LOGW(TAG, "reboot requested by controller");
    restart_soon();
    return NULL;
  }
  if (strcmp(cmd, "adopt") == 0) {
    const char *token = cJSON_GetStringValue(cJSON_GetObjectItem(msg, "token"));
    const char *name = cJSON_GetStringValue(cJSON_GetObjectItem(msg, "name"));
    const char *snap = cJSON_GetStringValue(cJSON_GetObjectItem(msg, "snap_host"));
    const cJSON *port = cJSON_GetObjectItem(msg, "snap_port");
    if (!token || strlen(token) < 16 || strlen(token) >= CIVI_TOKEN_MAX)
      return "bad token";
    if (nvs_write("token", token) != ESP_OK) return "saving token failed";
    nvs_write("adopt_code", NULL);
    if (snap && snap[0]) {
      settings_set_server_host(snap);
      if (cJSON_IsNumber(port)) settings_set_server_port(port->valueint);
      settings_set_mdns_enabled(false);
    }
    ESP_LOGW(TAG, "adopted as \"%s\" (audio from %s), restarting", name ? name : "",
             snap ? snap : "unchanged");
    note(SRV_ADOPTED, name);
    restart_soon();
    return NULL;
  }
  if (strcmp(cmd, "reset") == 0) {
    // Back to a blank node: no Wi-Fi, no token, no server. It reboots into
    // the setup network.
    nvs_handle_t h;
    if (nvs_open(CIVI_NVS_NS, NVS_READWRITE, &h) == ESP_OK) {
      nvs_erase_all(h);
      nvs_commit(h);
      nvs_close(h);
    }
    settings_clear_server_host();
    settings_clear_server_port();
    esp_wifi_restore();
    ESP_LOGW(TAG, "reset by controller: Wi-Fi and adoption erased, restarting into setup");
    restart_soon();
    return NULL;
  }
  if (strcmp(cmd, "forget") == 0) {
    if (nvs_write("token", NULL) != ESP_OK) return "erasing token failed";
    nvs_write("name", NULL);
    s_name[0] = 0;
    // Snapserver has no access control: stop playing this server's audio.
    settings_clear_server_host();
    settings_clear_server_port();
    ESP_LOGW(TAG, "forgotten by controller, back to setup (Wi-Fi kept)");
    restart_soon();
    return NULL;
  }
  if (strcmp(cmd, "set_server") == 0) {
    const char *host = cJSON_GetStringValue(cJSON_GetObjectItem(msg, "host"));
    const cJSON *port = cJSON_GetObjectItem(msg, "port");
    if (!host || !host[0]) return "host required";
    if (settings_set_server_host(host) != ESP_OK) return "saving host failed";
    if (cJSON_IsNumber(port) && settings_set_server_port(port->valueint) != ESP_OK)
      return "saving port failed";
    settings_set_mdns_enabled(false);
    ESP_LOGW(TAG, "snapserver -> %s, restarting", host);
    restart_soon();
    return NULL;
  }
  if (strcmp(cmd, "set_controller") == 0) {
    const char *url = cJSON_GetStringValue(cJSON_GetObjectItem(msg, "url"));
    if (!url || strncmp(url, "ws", 2) != 0 || strlen(url) >= CIVI_URL_MAX)
      return "url must be ws:// or wss://";
    if (nvs_write("ctrl_url", url) != ESP_OK) return "saving url failed";
    ESP_LOGW(TAG, "controller -> %s, restarting", url);
    restart_soon();
    return NULL;
  }
#ifndef CONFIG_CIVITONE_ROLE_SOURCE
  if (strcmp(cmd, "set_speakers") == 0) return set_speakers(msg);
  if (strcmp(cmd, "update_firmware") == 0) return update_firmware(msg);
#endif
  const char *err = NULL;
  if (civitone_board_command && civitone_board_command(cmd, msg, &err)) return err;
  return "unknown cmd";
}

static void on_close(int code, const char *reason, int reason_len) {
  char why[80];
  snprintf(why, sizeof why, "%.*s", reason_len, reason);
  if (code == CIVI_CLOSE_CODE_REJECTED) {
    // Don't keep dialling with a bad code: the controller rate-limits
    // wrong codes per address.
    ESP_LOGW(TAG, "controller refused the adopt code: %s", why);
    nvs_write("adopt_code", NULL);
    s_code[0] = 0;
    s_parked = true;
    note(SRV_REJECTED, why[0] ? why : "adopt code wrong or expired");
  } else if (code == CIVI_CLOSE_TOKEN_REJECTED && s_token[0]) {
    ESP_LOGW(TAG, "controller doesn't know our token; back to setup");
    nvs_write("token", NULL);
    nvs_write("name", NULL);
    s_name[0] = 0;
    settings_clear_server_host();  // stop playing that server's audio too
    settings_clear_server_port();
    s_token[0] = 0;
    s_parked = true;
    note(SRV_REJECTED, "this server no longer knows this node; enter a new adopt code");
  }
}

static void on_ws_event(void *arg, esp_event_base_t base, int32_t id, void *data) {
  esp_websocket_event_data_t *ev = (esp_websocket_event_data_t *)data;
  switch (id) {
    case WEBSOCKET_EVENT_CONNECTED:
      ESP_LOGI(TAG, "connected");
      send_hello();
      send_heartbeat();
      break;
    case WEBSOCKET_EVENT_DISCONNECTED:
      s_live = false;
      ESP_LOGW(TAG, "disconnected");
      if (!s_parked) note(SRV_ERROR, "can't reach the server; retrying");
      break;
    case WEBSOCKET_EVENT_DATA: {
      if (ev->op_code == 0x8) {  // close frame: 2-byte code, then reason
        if (ev->data_len >= 2) {
          int code = ((uint8_t)ev->data_ptr[0] << 8) | (uint8_t)ev->data_ptr[1];
          on_close(code, ev->data_ptr + 2, ev->data_len - 2);
        }
        break;
      }
      // Text frames only, and only whole ones; commands are small.
      if (ev->op_code != 0x1 || ev->payload_offset != 0 ||
          ev->data_len != ev->payload_len) {
        break;
      }
      cJSON *msg = cJSON_ParseWithLength(ev->data_ptr, ev->data_len);
      if (!msg) break;
      const char *t = cJSON_GetStringValue(cJSON_GetObjectItem(msg, "t"));
      const cJSON *cid = cJSON_GetObjectItem(msg, "id");
      if (t && strcmp(t, "cmd") == 0 && cJSON_IsNumber(cid)) {
        send_ack(cid->valuedouble, run_command(msg));
      } else if (t && strcmp(t, "welcome") == 0) {
        const char *st = cJSON_GetStringValue(cJSON_GetObjectItem(msg, "state"));
        bool adopted = st && strcmp(st, "adopted") == 0;
        if (adopted) s_session++;
        s_live = adopted;
#ifndef CONFIG_CIVITONE_ROLE_SOURCE
        if (adopted) confirm_firmware();
#endif
        const char *name = cJSON_GetStringValue(cJSON_GetObjectItem(msg, "name"));
        if (adopted && name && name[0] && strcmp(name, s_name) != 0) {
          snprintf(s_name, sizeof s_name, "%s", name);
          nvs_write("name", s_name);
        }
        ESP_LOGI(TAG, "controller says: %s", st ? st : "?");
        note(adopted ? SRV_ADOPTED : SRV_PENDING, adopted ? "" : "waiting for approval in the console");
      } else {
        ESP_LOGI(TAG, "ignoring frame t=%s", t ? t : "?");
      }
      cJSON_Delete(msg);
      break;
    }
    default:
      break;
  }
}

static bool have_ip(void) {
  esp_netif_t *netif = esp_netif_get_handle_from_ifkey("WIFI_STA_DEF");
  esp_netif_ip_info_t ip;
  return netif && esp_netif_get_ip_info(netif, &ip) == ESP_OK && ip.ip.addr != 0;
}

static void client_stop(void) {
  if (!s_client) return;
  s_live = false;
  xSemaphoreTake(s_client_lock, portMAX_DELAY);
  esp_websocket_client_stop(s_client);
  esp_websocket_client_destroy(s_client);
  s_client = NULL;
  xSemaphoreGive(s_client_lock);
}

// Builds and starts a client from what NVS says now. False if there is
// nothing to dial yet (setup not done).
static bool client_start(void) {
  static char url[CIVI_URL_MAX];
  nvs_read("token", s_token, sizeof s_token);
  nvs_read("adopt_code", s_code, sizeof s_code);
  if (!s_token[0] && !s_code[0]) return false;  // needs setup
  if (!controller_url(url, sizeof url)) return false;

  ESP_LOGI(TAG, "%s dialling %s (%s)", s_device_id, url, s_token[0] ? "adopted" : "with adopt code");
  esp_websocket_client_config_t cfg = {
      .uri = url,
      .reconnect_timeout_ms = 3000,
      .network_timeout_ms = 5000,
      .ping_interval_sec = 10,
      .task_stack = 6144,
      .buffer_size = 2048,
      // wss:// is verified against the public CA bundle (Let's Encrypt and
      // friends), which covers the hosted server and self-hosters with a
      // real certificate. ws:// on a LAN skips TLS entirely.
      .crt_bundle_attach = esp_crt_bundle_attach,
  };
  if (s_token[0]) {
    snprintf(s_auth_header, sizeof s_auth_header, "Authorization: Bearer %s\r\n", s_token);
    cfg.headers = s_auth_header;
  }
  xSemaphoreTake(s_client_lock, portMAX_DELAY);
  s_client = esp_websocket_client_init(&cfg);
  esp_websocket_register_events(s_client, WEBSOCKET_EVENT_ANY, on_ws_event, NULL);
  esp_websocket_client_start(s_client);
  xSemaphoreGive(s_client_lock);
  note(SRV_CONNECTING, url);
  return true;
}

static void ctrl_task(void *arg) {
  load_identity();
  int64_t last_hb = 0;
  for (;;) {
    vTaskDelay(pdMS_TO_TICKS(250));
    if (s_reload || s_parked) {
      client_stop();
      s_reload = false;
    }
    if (!s_client) {
      if (s_parked || !have_ip()) continue;
      if (!client_start()) {
        vTaskDelay(pdMS_TO_TICKS(1000));
        continue;
      }
    }
    int64_t now = esp_timer_get_time();
    if ((s_hb_now || now - last_hb >= CIVI_HB_MS * 1000LL) && esp_websocket_client_is_connected(s_client)) {
      last_hb = now;
      s_hb_now = false;
      send_heartbeat();
    }
  }
}

void civitone_ctrl_start(void) {
  nvs_read("name", s_name, sizeof s_name);  // the setup page shows it, Wi-Fi or not
#ifndef CONFIG_CIVITONE_ROLE_SOURCE
  load_speakers();
  if (pcm51xx_reapply_volume) pcm51xx_reapply_volume();  // in case the DAC already has a volume
#endif
  s_client_lock = xSemaphoreCreateMutex();
  xTaskCreate(ctrl_task, "civi_ctrl", 6144, NULL, 4, NULL);
}
