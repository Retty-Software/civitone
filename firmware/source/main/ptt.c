// KEY1 (GPIO36) is pulled up on the board and reads low while pressed.
// Every change is sent as {"t":"ptt","down":...}, and the current state is
// re-sent on each new session, so a press held through a reconnect still
// counts and a release during an outage isn't lost (the controller also
// ends a page when its mic disconnects).
//
// The green LED (GPIO22, active low) is lit while talk is held and the
// controller is connected: "you're live".

#include "ptt.h"

#include <stdio.h>

#include "civitone_ctrl.h"
#include "driver/gpio.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

static const char *TAG = "PTT";

#define PIN_KEY 36
#define PIN_LED 22
#define POLL_MS 10
#define DEBOUNCE_MS 30

static volatile bool s_down;

bool ptt_down(void) { return s_down; }

static void ptt_task(void *arg) {
  bool raw_last = false, sent = false;
  uint32_t sent_session = 0;
  int stable_ms = 0;
  for (;;) {
    vTaskDelay(pdMS_TO_TICKS(POLL_MS));
    bool raw = gpio_get_level(PIN_KEY) == 0;
    stable_ms = raw == raw_last ? stable_ms + POLL_MS : 0;
    raw_last = raw;
    if (stable_ms >= DEBOUNCE_MS && raw != s_down) {
      s_down = raw;
      ESP_LOGW(TAG, "talk %s", s_down ? "pressed" : "released");
    }
    uint32_t session = civitone_ctrl_session();
    gpio_set_level(PIN_LED, !(s_down && session));
    if (session && (session != sent_session || sent != s_down)) {
      char msg[40];
      snprintf(msg, sizeof msg, "{\"t\":\"ptt\",\"down\":%s}", s_down ? "true" : "false");
      if (civitone_ctrl_send_text(msg)) {
        sent = s_down;
        sent_session = session;
      }
    }
  }
}

void ptt_start(void) {
  gpio_config_t in = {.pin_bit_mask = 1ULL << PIN_KEY, .mode = GPIO_MODE_INPUT};
  gpio_config(&in);
  gpio_config_t out = {.pin_bit_mask = 1ULL << PIN_LED, .mode = GPIO_MODE_OUTPUT};
  gpio_config(&out);
  gpio_set_level(PIN_LED, 1);
  xTaskCreate(ptt_task, "ptt", 3072, NULL, 6, NULL);
}
