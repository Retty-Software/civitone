// Civitone setup mode. See docs/node-setup.md.
//
// The node keeps up to three Wi-Fi networks and tries them in turn. It opens
// an open "civitone-xxxx" network with a captive setup page when it has
// nowhere to report (no token and no adopt code), or when it has had no IP for
// CONFIG_CIVITONE_SETUP_AFTER_S. The setup network closes again once the node
// is on Wi-Fi and the controller has accepted it (pending or adopted), after a
// short delay so the page can show the result.

#include "civitone_setup.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "cJSON.h"
#include "civitone_ctrl.h"
#include "dns_hijack.h"
#include "esp_event.h"
#include "esp_http_server.h"
#include "esp_log.h"
#include "esp_mac.h"
#include "esp_netif.h"
#include "esp_timer.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "lwip/sockets.h"
#include "nvs.h"

static const char *TAG = "CIVI_SETUP";

#define NS "civitone"
#define MAX_NETS 3
#define FAILS_BEFORE_NEXT_NET 4
#define CLOSE_AFTER_OK_US (15 * 1000000LL)

extern const char setup_html_start[] asm("_binary_setup_html_start");
extern const char setup_html_end[] asm("_binary_setup_html_end");

typedef struct {
  char ssid[33];
  char pass[65];
} net_t;

static net_t s_nets[MAX_NETS];
static int s_n_nets;
static int s_cur_net;
static int s_fails;
static int s_last_reason;
static volatile bool s_hold_reconnect;

static bool s_ap_on;
static esp_netif_t *s_ap_netif;
static char s_ap_ssid[20];
static int64_t s_last_ip_us;  // last time we had an IP (or boot)
static bool s_have_ip;
static char s_ip[16];

static civi_srv_state_t s_srv = CIVI_SRV_IDLE;
static char s_srv_detail[96];
static int64_t s_srv_ok_since_us;  // PENDING/ADOPTED since

// --- NVS ---------------------------------------------------------------------

static bool nvs_str(const char *key, char *out, size_t len) {
  nvs_handle_t h;
  out[0] = 0;
  if (nvs_open(NS, NVS_READONLY, &h) != ESP_OK) return false;
  size_t n = len;
  esp_err_t err = nvs_get_str(h, key, out, &n);
  nvs_close(h);
  return err == ESP_OK && out[0];
}

static bool nvs_has(const char *key) {
  char buf[96];
  return nvs_str(key, buf, sizeof buf);
}

static void save_nets(void) {
  nvs_handle_t h;
  if (nvs_open(NS, NVS_READWRITE, &h) != ESP_OK) return;
  char k[12];
  for (int i = 0; i < MAX_NETS; i++) {
    snprintf(k, sizeof k, "net%d_ssid", i);
    if (i < s_n_nets) nvs_set_str(h, k, s_nets[i].ssid); else nvs_erase_key(h, k);
    snprintf(k, sizeof k, "net%d_pass", i);
    if (i < s_n_nets) nvs_set_str(h, k, s_nets[i].pass); else nvs_erase_key(h, k);
  }
  nvs_commit(h);
  nvs_close(h);
}

static void load_nets(void) {
  char k[12];
  s_n_nets = 0;
  for (int i = 0; i < MAX_NETS; i++) {
    snprintf(k, sizeof k, "net%d_ssid", i);
    if (!nvs_str(k, s_nets[s_n_nets].ssid, sizeof s_nets[0].ssid)) continue;
    snprintf(k, sizeof k, "net%d_pass", i);
    nvs_str(k, s_nets[s_n_nets].pass, sizeof s_nets[0].pass);
    s_n_nets++;
  }
  if (s_n_nets == 0) {
    // Nodes provisioned before setup mode existed (Improv, menuconfig) keep
    // their network: adopt whatever the Wi-Fi driver already has.
    wifi_config_t cfg;
    if (esp_wifi_get_config(WIFI_IF_STA, &cfg) == ESP_OK && cfg.sta.ssid[0]) {
      strlcpy(s_nets[0].ssid, (char *)cfg.sta.ssid, sizeof s_nets[0].ssid);
      strlcpy(s_nets[0].pass, (char *)cfg.sta.password, sizeof s_nets[0].pass);
      s_n_nets = 1;
      save_nets();
      ESP_LOGI(TAG, "imported Wi-Fi network \"%s\"", s_nets[0].ssid);
    }
  }
}

// Front of the list, no duplicates, at most MAX_NETS.
static void remember_net(const char *ssid, const char *pass) {
  net_t keep[MAX_NETS];
  int n = 0;
  strlcpy(keep[n].ssid, ssid, sizeof keep[0].ssid);
  strlcpy(keep[n].pass, pass, sizeof keep[0].pass);
  n++;
  for (int i = 0; i < s_n_nets && n < MAX_NETS; i++) {
    if (strcmp(s_nets[i].ssid, ssid) != 0) keep[n++] = s_nets[i];
  }
  memcpy(s_nets, keep, sizeof keep);
  s_n_nets = n;
  save_nets();
}

static void nvs_put(const char *key, const char *val) {
  nvs_handle_t h;
  if (nvs_open(NS, NVS_READWRITE, &h) != ESP_OK) return;
  if (val) nvs_set_str(h, key, val); else nvs_erase_key(h, key);
  nvs_commit(h);
  nvs_close(h);
}

// --- Wi-Fi station -------------------------------------------------------------

// While the setup network is up, the station retries slowly (monitor_task):
// every connect attempt sweeps channels and knocks phones off the setup AP.
bool civitone_setup_should_reconnect(void) { return !s_hold_reconnect && !s_ap_on; }

static void use_net(int i) {
  if (i >= s_n_nets) return;
  s_cur_net = i;
  s_fails = 0;
  wifi_config_t cfg;
  esp_wifi_get_config(WIFI_IF_STA, &cfg);
  memset(cfg.sta.ssid, 0, sizeof cfg.sta.ssid);
  memset(cfg.sta.password, 0, sizeof cfg.sta.password);
  memcpy(cfg.sta.ssid, s_nets[i].ssid, strlen(s_nets[i].ssid));
  memcpy(cfg.sta.password, s_nets[i].pass, strlen(s_nets[i].pass));
  cfg.sta.threshold.authmode = s_nets[i].pass[0] ? WIFI_AUTH_WPA_PSK : WIFI_AUTH_OPEN;
  cfg.sta.pmf_cfg.capable = true;
  cfg.sta.pmf_cfg.required = false;
  ESP_LOGI(TAG, "joining \"%s\" (%d of %d)", s_nets[i].ssid, i + 1, s_n_nets);
  s_hold_reconnect = true;
  esp_wifi_disconnect();
  esp_wifi_set_config(WIFI_IF_STA, &cfg);
  s_hold_reconnect = false;
  esp_wifi_connect();
}

static void on_wifi(void *arg, esp_event_base_t base, int32_t id, void *data) {
  if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
    wifi_event_sta_disconnected_t *d = data;
    s_last_reason = d->reason;
    s_have_ip = false;
    if (s_hold_reconnect) return;
    if (++s_fails >= FAILS_BEFORE_NEXT_NET && s_n_nets > 1) {
      use_net((s_cur_net + 1) % s_n_nets);
    }
  } else if (base == IP_EVENT && id == IP_EVENT_STA_GOT_IP) {
    ip_event_got_ip_t *e = data;
    s_fails = 0;
    s_last_reason = 0;
    s_have_ip = true;
    s_last_ip_us = esp_timer_get_time();
    snprintf(s_ip, sizeof s_ip, IPSTR, IP2STR(&e->ip_info.ip));
  } else if (base == IP_EVENT && id == IP_EVENT_STA_LOST_IP) {
    s_have_ip = false;
  }
}

// --- setup network -------------------------------------------------------------

static void web_start(void);
static void web_stop(void);

static void ap_start(void) {
  if (s_ap_on) return;
  if (!s_ap_netif) s_ap_netif = esp_netif_create_default_wifi_ap();
  esp_wifi_set_mode(WIFI_MODE_APSTA);
  wifi_config_t ap = {.ap = {.max_connection = 4, .authmode = WIFI_AUTH_OPEN}};
  strlcpy((char *)ap.ap.ssid, s_ap_ssid, sizeof ap.ap.ssid);
  ap.ap.ssid_len = strlen(s_ap_ssid);
  esp_wifi_set_config(WIFI_IF_AP, &ap);
  esp_netif_ip_info_t ip;
  esp_netif_get_ip_info(s_ap_netif, &ip);
  dns_hijack_start(ip.ip.addr);
  web_start();
  s_ap_on = true;
  ESP_LOGW(TAG, "setup network \"%s\" is up: join it and open http://" IPSTR "/setup",
           s_ap_ssid, IP2STR(&ip.ip));
}

static void ap_stop(void) {
  if (!s_ap_on) return;
  dns_hijack_stop();
  web_stop();
  esp_wifi_set_mode(WIFI_MODE_STA);
  s_ap_on = false;
  ESP_LOGI(TAG, "setup network closed");
}

void civitone_setup_note_server(civi_srv_state_t state, const char *detail) {
  if (state != s_srv) {
    s_srv_ok_since_us = (state == CIVI_SRV_PENDING || state == CIVI_SRV_ADOPTED) ? esp_timer_get_time() : 0;
  }
  s_srv = state;
  strlcpy(s_srv_detail, detail ? detail : "", sizeof s_srv_detail);
}

static void monitor_task(void *arg) {
  int64_t last_try = 0;
  for (;;) {
    vTaskDelay(pdMS_TO_TICKS(1000));
    int64_t now = esp_timer_get_time();
    if (s_ap_on && !s_have_ip && s_n_nets && !s_hold_reconnect && now - last_try > 10000000) {
      last_try = now;
      esp_wifi_connect();
    }
    bool configured = nvs_has("token") || nvs_has("adopt_code");
    bool offline_too_long = !s_have_ip && now - s_last_ip_us > (int64_t)CONFIG_CIVITONE_SETUP_AFTER_S * 1000000;
    if (s_n_nets == 0 || !configured || offline_too_long) {
      ap_start();
    } else if (s_ap_on && s_have_ip && s_srv_ok_since_us && now - s_srv_ok_since_us > CLOSE_AFTER_OK_US) {
      ap_stop();
    }
  }
}

// --- setup page and API ----------------------------------------------------------

static esp_err_t send_json(httpd_req_t *req, cJSON *o) {
  char *s = cJSON_PrintUnformatted(o);
  httpd_resp_set_type(req, "application/json");
  httpd_resp_set_hdr(req, "Cache-Control", "no-store");
  esp_err_t err = httpd_resp_sendstr(req, s ? s : "{}");
  cJSON_free(s);
  cJSON_Delete(o);
  return err;
}

static esp_err_t page_get(httpd_req_t *req) {
  httpd_resp_set_type(req, "text/html");
  httpd_resp_set_hdr(req, "Cache-Control", "no-store");
  return httpd_resp_send(req, setup_html_start, setup_html_end - setup_html_start);
}

// Phones probe a known URL after joining a network; anything but the
// expected answer makes them open the captive page.
static esp_err_t captive_get(httpd_req_t *req) {
  httpd_resp_set_status(req, "302 Found");
  httpd_resp_set_hdr(req, "Location", "http://192.168.4.1/setup");
  return httpd_resp_send(req, NULL, 0);
}

static esp_err_t info_get(httpd_req_t *req) {
  cJSON *o = cJSON_CreateObject();
  cJSON_AddStringToObject(o, "device_id", civitone_ctrl_device_id());
  cJSON_AddStringToObject(o, "name", civitone_ctrl_name());
  cJSON_AddStringToObject(o, "fw", civitone_ctrl_fw_version());
#ifdef CONFIG_CIVITONE_BOARD
  cJSON_AddStringToObject(o, "board", CONFIG_CIVITONE_BOARD);
#endif
#ifdef CONFIG_CIVITONE_ROLE_SOURCE
  cJSON_AddStringToObject(o, "role", "source");
#else
  cJSON_AddStringToObject(o, "role", "speaker");
#endif
  uint8_t mac[6];
  char macs[18];
  esp_read_mac(mac, ESP_MAC_WIFI_STA);
  snprintf(macs, sizeof macs, "%02X:%02X:%02X:%02X:%02X:%02X", mac[0], mac[1], mac[2], mac[3], mac[4], mac[5]);
  cJSON_AddStringToObject(o, "mac", macs);
  char server[128];
  if (!nvs_str("server", server, sizeof server)) strlcpy(server, CONFIG_CIVITONE_DEFAULT_SERVER, sizeof server);
  cJSON_AddStringToObject(o, "server", server);
  cJSON_AddBoolToObject(o, "adopted", nvs_has("token"));
  cJSON *nets = cJSON_AddArrayToObject(o, "networks");
  for (int i = 0; i < s_n_nets; i++) cJSON_AddItemToArray(nets, cJSON_CreateString(s_nets[i].ssid));
  return send_json(req, o);
}

static int cmp_rssi(const void *a, const void *b) {
  return ((const wifi_ap_record_t *)b)->rssi - ((const wifi_ap_record_t *)a)->rssi;
}

static esp_err_t scan_get(httpd_req_t *req) {
  // A station that is still trying to join can't scan: pause it.
  bool paused = false;
  if (!s_have_ip) {
    s_hold_reconnect = paused = true;
    esp_wifi_disconnect();
    vTaskDelay(pdMS_TO_TICKS(100));
  }
  wifi_scan_config_t sc = {.show_hidden = false};
  esp_err_t err = esp_wifi_scan_start(&sc, true);
  uint16_t n = 0;
  wifi_ap_record_t *recs = NULL;
  if (err == ESP_OK) {
    esp_wifi_scan_get_ap_num(&n);
    if (n > 30) n = 30;
    recs = calloc(n ? n : 1, sizeof *recs);
    if (recs) esp_wifi_scan_get_ap_records(&n, recs);
  }
  if (paused) {
    s_hold_reconnect = false;
    esp_wifi_connect();
  }

  cJSON *arr = cJSON_CreateArray();
  if (recs) {
    qsort(recs, n, sizeof *recs, cmp_rssi);
    for (int i = 0; i < n; i++) {
      if (!recs[i].ssid[0]) continue;
      bool dup = false;
      for (int j = 0; j < i; j++) dup |= strcmp((char *)recs[i].ssid, (char *)recs[j].ssid) == 0;
      if (dup) continue;
      cJSON *r = cJSON_CreateObject();
      cJSON_AddStringToObject(r, "ssid", (char *)recs[i].ssid);
      cJSON_AddNumberToObject(r, "rssi", recs[i].rssi);
      cJSON_AddBoolToObject(r, "secure", recs[i].authmode != WIFI_AUTH_OPEN);
      cJSON_AddItemToArray(arr, r);
    }
    free(recs);
  } else {
    ESP_LOGW(TAG, "scan failed: %s", esp_err_to_name(err));
  }
  cJSON *o = cJSON_CreateObject();
  cJSON_AddItemToObject(o, "networks", arr);
  return send_json(req, o);
}

// "civitone.example.com" -> wss://civitone.example.com/node (public, TLS)
// "192.168.1.10:8080" or "pi.local:8080" -> ws://.../node (LAN)
// ws://, wss://, http://, https:// are taken as given.
static bool controller_url_for(const char *server, char *out, size_t len) {
  const char *s = server;
  while (*s == ' ') s++;
  if (!*s) return false;
  const char *scheme = NULL, *rest = s;
  if (!strncmp(s, "wss://", 6)) scheme = "wss", rest = s + 6;
  else if (!strncmp(s, "ws://", 5)) scheme = "ws", rest = s + 5;
  else if (!strncmp(s, "https://", 8)) scheme = "wss", rest = s + 8;
  else if (!strncmp(s, "http://", 7)) scheme = "ws", rest = s + 7;
  else scheme = strchr(s, ':') ? "ws" : "wss";
  char host[96];
  strlcpy(host, rest, sizeof host);
  char *slash = strchr(host, '/');
  if (slash) *slash = 0;  // path is always /node
  size_t hl = strlen(host);
  while (hl && host[hl - 1] == ' ') host[--hl] = 0;
  if (!hl) return false;
  return snprintf(out, len, "%s://%s/node", scheme, host) < (int)len;
}

static esp_err_t save_post(httpd_req_t *req) {
  char body[512];
  int n = httpd_req_recv(req, body, sizeof body - 1);
  if (n <= 0) return httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "empty body");
  body[n] = 0;
  cJSON *in = cJSON_Parse(body);
  const char *ssid = cJSON_GetStringValue(cJSON_GetObjectItem(in, "ssid"));
  const char *pass = cJSON_GetStringValue(cJSON_GetObjectItem(in, "password"));
  const char *server = cJSON_GetStringValue(cJSON_GetObjectItem(in, "server"));
  const char *code = cJSON_GetStringValue(cJSON_GetObjectItem(in, "code"));
  char url[160];
  const char *err = NULL;
  // An adopted node can change Wi-Fi alone and keep its adoption. A code
  // means (re)adopting, on this server or a new one.
  bool readopt = code && code[0];
  bool adopted = nvs_has("token");
  if (!ssid || !ssid[0] || strlen(ssid) > 32) err = "Pick a Wi-Fi network.";
  else if (pass && strlen(pass) > 64) err = "That password is too long.";
  else if (pass && pass[0] && strlen(pass) < 8) err = "Wi-Fi passwords are at least 8 characters.";
  else if (!readopt && !adopted) err = "Enter the adopt code from the console.";
  else if (readopt && (!server || !controller_url_for(server, url, sizeof url))) err = "Enter the server address.";
  else if (readopt && (strlen(code) < 6 || strlen(code) > 12)) err = "That adopt code looks wrong.";
  if (err) {
    cJSON_Delete(in);
    httpd_resp_set_status(req, "400 Bad Request");
    return httpd_resp_sendstr(req, err);
  }

  // An empty password for a network we already know keeps the saved one,
  // so re-running setup doesn't need the Wi-Fi password again.
  const char *use_pass = pass ? pass : "";
  if (!use_pass[0]) {
    for (int i = 0; i < s_n_nets; i++) {
      if (strcmp(s_nets[i].ssid, ssid) == 0) use_pass = s_nets[i].pass;
    }
  }
  char keep[65];
  strlcpy(keep, use_pass, sizeof keep);  // remember_net reorders s_nets
  remember_net(ssid, keep);
  if (readopt) {
    nvs_put("server", server);
    nvs_put("ctrl_url", url);
    nvs_put("adopt_code", code);
    nvs_put("token", NULL);  // a new code means a new adoption, here or elsewhere
    ESP_LOGW(TAG, "setup saved: wifi \"%s\", controller %s, re-adopting", ssid, url);
    civitone_setup_note_server(CIVI_SRV_IDLE, "");
  } else {
    ESP_LOGW(TAG, "setup saved: wifi \"%s\", keeping adoption", ssid);
  }
  cJSON_Delete(in);

  use_net(0);
  if (readopt) civitone_ctrl_reload();
  return send_json(req, cJSON_CreateObject());
}

// {"ssid":"..."}: a saved network goes to the front of the list and is
// tried now (its saved password), for "Use now" on an adopted node.
static esp_err_t use_post(httpd_req_t *req) {
  char body[96];
  int n = httpd_req_recv(req, body, sizeof body - 1);
  if (n <= 0) return httpd_resp_send_err(req, HTTPD_400_BAD_REQUEST, "empty body");
  body[n] = 0;
  cJSON *in = cJSON_Parse(body);
  const char *ssid = cJSON_GetStringValue(cJSON_GetObjectItem(in, "ssid"));
  int found = -1;
  for (int i = 0; ssid && i < s_n_nets; i++) {
    if (strcmp(s_nets[i].ssid, ssid) == 0) found = i;
  }
  if (found < 0) {
    cJSON_Delete(in);
    httpd_resp_set_status(req, "400 Bad Request");
    return httpd_resp_sendstr(req, "That network isn't saved.");
  }
  net_t pick = s_nets[found];
  cJSON_Delete(in);
  remember_net(pick.ssid, pick.pass);
  ESP_LOGW(TAG, "setup: use saved network \"%s\" first", pick.ssid);
  use_net(0);
  return send_json(req, cJSON_CreateObject());
}

static const char *reason_text(int r) {
  switch (r) {
    case 0: return "";
    case WIFI_REASON_NO_AP_FOUND: return "network not found";
    case WIFI_REASON_AUTH_FAIL:
    case WIFI_REASON_4WAY_HANDSHAKE_TIMEOUT:
    case WIFI_REASON_HANDSHAKE_TIMEOUT: return "wrong password?";
    case WIFI_REASON_ASSOC_FAIL: return "the access point refused the connection";
    default: return "could not connect";
  }
}

static esp_err_t status_get(httpd_req_t *req) {
  cJSON *o = cJSON_CreateObject();
  cJSON *w = cJSON_AddObjectToObject(o, "wifi");
  cJSON_AddStringToObject(w, "ssid", s_n_nets ? s_nets[s_cur_net].ssid : "");
  cJSON_AddStringToObject(w, "state", s_have_ip ? "connected" : s_fails > 1 ? "failing" : "connecting");
  cJSON_AddStringToObject(w, "ip", s_have_ip ? s_ip : "");
  cJSON_AddStringToObject(w, "problem", s_have_ip ? "" : reason_text(s_last_reason));
  cJSON_AddNumberToObject(w, "fails", s_fails);
  wifi_ap_record_t ap;
  if (s_have_ip && esp_wifi_sta_get_ap_info(&ap) == ESP_OK) cJSON_AddNumberToObject(w, "rssi", ap.rssi);
  cJSON_AddStringToObject(o, "name", civitone_ctrl_name());
  static const char *names[] = {"idle", "connecting", "pending", "adopted", "rejected", "error"};
  cJSON *s = cJSON_AddObjectToObject(o, "server");
  cJSON_AddStringToObject(s, "state", names[s_srv]);
  cJSON_AddStringToObject(s, "detail", s_srv_detail);
  int closing = 0;
  if (s_ap_on && s_have_ip && s_srv_ok_since_us) {
    closing = (int)((CLOSE_AFTER_OK_US - (esp_timer_get_time() - s_srv_ok_since_us)) / 1000000);
    if (closing < 0) closing = 0;
  }
  cJSON_AddNumberToObject(o, "closing_in", closing);
  return send_json(req, o);
}

// The setup page has its own web server, up only while the setup network
// is (snapclient's web UI is off: patch 0012). The setup network runs beside
// the node's normal Wi-Fi, so each request must also have arrived on the
// setup network's address: nobody on the LAN can change the node's Wi-Fi,
// server or adopt code.
static httpd_handle_t s_httpd;

static bool on_setup_network(httpd_req_t *req) {
  struct sockaddr_storage a;
  socklen_t len = sizeof a;
  esp_netif_ip_info_t ip;
  if (!s_ap_netif || esp_netif_get_ip_info(s_ap_netif, &ip) != ESP_OK) return false;
  if (getsockname(httpd_req_to_sockfd(req), (struct sockaddr *)&a, &len) != 0) return false;
  if (a.ss_family == AF_INET) return ((struct sockaddr_in *)&a)->sin_addr.s_addr == ip.ip.addr;
  if (a.ss_family == AF_INET6) {  // the server listens dual-stack: IPv4 shows up mapped, ::ffff:a.b.c.d
    const uint8_t *b = (const uint8_t *)&((struct sockaddr_in6 *)&a)->sin6_addr;
    static const uint8_t mapped[12] = {0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff};
    uint32_t v4;
    memcpy(&v4, b + 12, 4);
    return memcmp(b, mapped, 12) == 0 && v4 == ip.ip.addr;
  }
  return false;
}

// Anything else on the setup network goes to the setup page (captive portal).
static esp_err_t to_setup(httpd_req_t *req, httpd_err_code_t err) {
  httpd_resp_set_status(req, "302 Found");
  httpd_resp_set_hdr(req, "Location", "http://192.168.4.1/setup");
  return httpd_resp_send(req, NULL, 0);
}

static esp_err_t guarded(httpd_req_t *req) {
  if (!on_setup_network(req)) {
    return httpd_resp_send_err(req, HTTPD_403_FORBIDDEN, "Setup is only on the node's own setup network.");
  }
  return ((esp_err_t (*)(httpd_req_t *))req->user_ctx)(req);
}

static void web_start(void) {
  if (s_httpd) return;
  static const struct {
    const char *uri;
    httpd_method_t method;
    esp_err_t (*fn)(httpd_req_t *);
  } routes[] = {
      {"/setup", HTTP_GET, page_get},
      {"/setup/info", HTTP_GET, info_get},
      {"/setup/scan", HTTP_GET, scan_get},
      {"/setup/status", HTTP_GET, status_get},
      {"/setup/save", HTTP_POST, save_post},
      {"/setup/use", HTTP_POST, use_post},
      {"/", HTTP_GET, captive_get},
      {"/generate_204", HTTP_GET, captive_get},
      {"/gen_204", HTTP_GET, captive_get},
      {"/hotspot-detect.html", HTTP_GET, captive_get},
      {"/library/test/success.html", HTTP_GET, captive_get},
      {"/connecttest.txt", HTTP_GET, captive_get},
      {"/ncsi.txt", HTTP_GET, captive_get},
      {"/success.txt", HTTP_GET, captive_get},
      {"/canonical.html", HTTP_GET, captive_get},
  };
  httpd_config_t cfg = HTTPD_DEFAULT_CONFIG();
  cfg.max_uri_handlers = sizeof routes / sizeof routes[0];
  cfg.lru_purge_enable = true;
  if (httpd_start(&s_httpd, &cfg) != ESP_OK) {
    ESP_LOGE(TAG, "setup page: web server didn't start");
    s_httpd = NULL;
    return;
  }
  for (size_t i = 0; i < sizeof routes / sizeof routes[0]; i++) {
    httpd_uri_t u = {.uri = routes[i].uri, .method = routes[i].method, .handler = guarded, .user_ctx = (void *)routes[i].fn};
    httpd_register_uri_handler(s_httpd, &u);
  }
  httpd_register_err_handler(s_httpd, HTTPD_404_NOT_FOUND, to_setup);
}

static void web_stop(void) {
  if (!s_httpd) return;
  httpd_stop(s_httpd);
  s_httpd = NULL;
}

// --- start ---------------------------------------------------------------------

void civitone_setup_start(void) {
  uint8_t mac[6];
  esp_read_mac(mac, ESP_MAC_WIFI_STA);
  snprintf(s_ap_ssid, sizeof s_ap_ssid, "civitone-%02x%02x", mac[4], mac[5]);
  s_last_ip_us = esp_timer_get_time();

  esp_event_handler_register(WIFI_EVENT, WIFI_EVENT_STA_DISCONNECTED, on_wifi, NULL);
  esp_event_handler_register(IP_EVENT, IP_EVENT_STA_GOT_IP, on_wifi, NULL);
  esp_event_handler_register(IP_EVENT, IP_EVENT_STA_LOST_IP, on_wifi, NULL);

  load_nets();
  if (s_n_nets) {
    // The driver may already be joining its stored network, which is our
    // first one anyway; only switch if it differs.
    wifi_config_t cfg;
    esp_wifi_get_config(WIFI_IF_STA, &cfg);
    if (strcmp((char *)cfg.sta.ssid, s_nets[0].ssid) != 0) use_net(0);
  }
  xTaskCreate(monitor_task, "civi_setup", 3072, NULL, 3, NULL);
}
