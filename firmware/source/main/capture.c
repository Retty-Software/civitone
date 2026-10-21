// The ES8388 records stereo at 48 kHz. Each 20 ms block is downmixed to mono
// (the format sources send, docs/control-protocol.md) and queued; a separate
// task sends the queue so a slow network never stalls the I2S reads. While
// the board isn't adopted and connected, blocks are read and dropped.
//
// Input (mic or line) and gain are saved in NVS and changed from the
// controller with the "set_input" command:
//   {"cmd":"set_input","input":"line"|"mic","gain_db":0..24}
// gain_db is optional; changing input without it uses that input's default.

#include "capture.h"

#include <math.h>
#include <stdlib.h>
#include <string.h>

#include "cJSON.h"
#include "civitone_ctrl.h"
#include "driver/i2s_std.h"
#include "es8388.h"
#include "ptt.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/ringbuf.h"
#include "freertos/task.h"
#include "nvs.h"

static const char *TAG = "CAPTURE";

// ESP32-A1S Audio Kit v2.2 (ES8388 variant).
#define PIN_SDA 33
#define PIN_SCL 32
#define PIN_MCLK 0
#define PIN_BCLK 27
#define PIN_WS 25
#define PIN_DIN 35

#define RATE 48000
#define BLOCK 960  // 20 ms
#define QUEUE_BLOCKS 25
#define GAIN_MIC_DB 12  // close speech peaked at -12 dBFS with 0 dB
#define GAIN_LINE_DB 0

static i2s_chan_handle_t s_rx;
static RingbufHandle_t s_queue;
static es8388_input_t s_input = ES8388_IN_LINE;
static int s_gain_db = GAIN_LINE_DB;
static volatile int s_peak;  // since the last heartbeat
static volatile uint32_t s_sent, s_dropped;

static void load_settings(void) {
  nvs_handle_t h;
  if (nvs_open("civitone", NVS_READONLY, &h) != ESP_OK) return;
  char in[8] = {0};
  size_t n = sizeof in;
  if (nvs_get_str(h, "input", in, &n) == ESP_OK) {
    s_input = strcmp(in, "mic") == 0 ? ES8388_IN_MIC : ES8388_IN_LINE;
    s_gain_db = s_input == ES8388_IN_MIC ? GAIN_MIC_DB : GAIN_LINE_DB;
  }
  int32_t g;
  if (nvs_get_i32(h, "gain_db", &g) == ESP_OK) s_gain_db = g;
  nvs_close(h);
}

static void save_settings(void) {
  nvs_handle_t h;
  if (nvs_open("civitone", NVS_READWRITE, &h) != ESP_OK) return;
  nvs_set_str(h, "input", s_input == ES8388_IN_MIC ? "mic" : "line");
  nvs_set_i32(h, "gain_db", s_gain_db);
  nvs_commit(h);
  nvs_close(h);
}

bool civitone_board_command(const char *cmd, const cJSON *msg, const char **err) {
  if (strcmp(cmd, "set_input") != 0) return false;
  const char *in = cJSON_GetStringValue(cJSON_GetObjectItem(msg, "input"));
  const cJSON *gain = cJSON_GetObjectItem(msg, "gain_db");
  es8388_input_t want = s_input;
  if (in) {
    if (strcmp(in, "mic") == 0) want = ES8388_IN_MIC;
    else if (strcmp(in, "line") == 0) want = ES8388_IN_LINE;
    else {
      *err = "input must be line or mic";
      return true;
    }
  }
  int db = want != s_input ? (want == ES8388_IN_MIC ? GAIN_MIC_DB : GAIN_LINE_DB) : s_gain_db;
  if (cJSON_IsNumber(gain)) {
    if (gain->valueint < 0 || gain->valueint > 24) {
      *err = "gain_db must be 0..24";
      return true;
    }
    db = gain->valueint;
  }
  if (es8388_set_input(want) != ESP_OK || es8388_set_gain_db(db) != ESP_OK) {
    *err = "codec write failed";
    return true;
  }
  s_input = want;
  s_gain_db = db / 3 * 3;
  save_settings();
  ESP_LOGW(TAG, "input %s, gain %d dB", s_input == ES8388_IN_MIC ? "mic" : "line", s_gain_db);
  *err = NULL;
  return true;
}

static double dbfs(int peak) { return peak > 0 ? round(200 * log10(peak / 32768.0)) / 10 : -99; }

void civitone_board_heartbeat(cJSON *hb) {
  cJSON_AddStringToObject(hb, "input", s_input == ES8388_IN_MIC ? "mic" : "line");
  cJSON_AddNumberToObject(hb, "gain_db", s_gain_db);
  cJSON_AddNumberToObject(hb, "peak_dbfs", dbfs(s_peak));
  cJSON_AddNumberToObject(hb, "sent", s_sent);
  cJSON_AddNumberToObject(hb, "dropped", s_dropped);
  cJSON_AddBoolToObject(hb, "ptt", ptt_down());
  s_peak = 0;
}

static void read_task(void *arg) {
  static int16_t st[BLOCK * 2];
  int16_t mono[BLOCK];
  int peak = 0;
  for (int blocks = 1;; blocks++) {
    size_t got = 0;
    if (i2s_channel_read(s_rx, st, sizeof st, &got, portMAX_DELAY) != ESP_OK || got != sizeof st) continue;
    for (int i = 0; i < BLOCK; i++) {
      int v = (st[2 * i] + st[2 * i + 1]) / 2;
      mono[i] = (int16_t)v;
      if (abs(v) > peak) peak = abs(v);
    }
    if (peak > s_peak) s_peak = peak;
    if (blocks % 250 == 0) {  // every 5 s, for the serial log
      ESP_LOGI(TAG, "%s +%d dB: peak %.1f dBFS, sent %lu, dropped %lu",
               s_input == ES8388_IN_MIC ? "mic" : "line", s_gain_db, dbfs(peak),
               (unsigned long)s_sent, (unsigned long)s_dropped);
      peak = 0;
    }
    if (xRingbufferSend(s_queue, mono, sizeof mono, 0) != pdTRUE) s_dropped++;
  }
}

static void send_task(void *arg) {
  for (;;) {
    size_t n;
    void *blk = xRingbufferReceive(s_queue, &n, portMAX_DELAY);
    if (!blk) continue;
    if (civitone_ctrl_send_audio(blk, n)) s_sent++;
    vRingbufferReturnItem(s_queue, blk);
  }
}

void capture_start(void) {
  load_settings();

  i2s_chan_config_t cc = I2S_CHANNEL_DEFAULT_CONFIG(I2S_NUM_0, I2S_ROLE_MASTER);
  cc.dma_desc_num = 6;
  cc.dma_frame_num = 480;
  ESP_ERROR_CHECK(i2s_new_channel(&cc, NULL, &s_rx));
  i2s_std_config_t sc = {
      .clk_cfg = I2S_STD_CLK_DEFAULT_CONFIG(RATE),
      .slot_cfg = I2S_STD_PHILIPS_SLOT_DEFAULT_CONFIG(I2S_DATA_BIT_WIDTH_16BIT, I2S_SLOT_MODE_STEREO),
      .gpio_cfg = {.mclk = PIN_MCLK, .bclk = PIN_BCLK, .ws = PIN_WS, .dout = I2S_GPIO_UNUSED, .din = PIN_DIN},
  };
  sc.clk_cfg.clk_src = I2S_CLK_SRC_APLL;  // exact 12.288 MHz MCLK
  sc.clk_cfg.mclk_multiple = I2S_MCLK_MULTIPLE_256;
  ESP_ERROR_CHECK(i2s_channel_init_std_mode(s_rx, &sc));
  ESP_ERROR_CHECK(i2s_channel_enable(s_rx));  // MCLK running before the codec starts

  if (es8388_init(PIN_SDA, PIN_SCL) != ESP_OK || es8388_set_input(s_input) != ESP_OK ||
      es8388_set_gain_db(s_gain_db) != ESP_OK) {
    ESP_LOGE(TAG, "codec setup failed; sending silence");
  }
  ESP_LOGI(TAG, "capturing %s at +%d dB", s_input == ES8388_IN_MIC ? "mic" : "line", s_gain_db);

  s_queue = xRingbufferCreate(QUEUE_BLOCKS * (BLOCK * 2 + 8), RINGBUF_TYPE_NOSPLIT);
  xTaskCreatePinnedToCore(read_task, "capture", 4096, NULL, 10, NULL, 1);
  xTaskCreatePinnedToCore(send_task, "send", 4096, NULL, 5, NULL, 0);
}
