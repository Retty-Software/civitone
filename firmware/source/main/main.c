// Civitone input board: Wi-Fi with the Civitone setup network, the control
// channel as a source, and codec capture. See docs/node-setup.md.

#include "capture.h"
#include "ptt.h"
#include "civitone_ctrl.h"
#include "civitone_setup.h"
#include "esp_event.h"
#include "esp_log.h"
#include "esp_netif.h"
#include "esp_wifi.h"
#include "nvs_flash.h"

static const char *TAG = "SOURCE";

static void on_wifi(void *arg, esp_event_base_t base, int32_t id, void *data) {
  if (id == WIFI_EVENT_STA_START) {
    esp_wifi_connect();
  } else if (id == WIFI_EVENT_STA_DISCONNECTED) {
    if (civitone_setup_should_reconnect()) esp_wifi_connect();
  }
}

void app_main(void) {
  esp_err_t err = nvs_flash_init();
  if (err == ESP_ERR_NVS_NO_FREE_PAGES || err == ESP_ERR_NVS_NEW_VERSION_FOUND) {
    nvs_flash_erase();
    err = nvs_flash_init();
  }
  ESP_ERROR_CHECK(err);
  ESP_LOGI(TAG, "Civitone source %s", civitone_ctrl_device_id());

  ESP_ERROR_CHECK(esp_netif_init());
  ESP_ERROR_CHECK(esp_event_loop_create_default());
  esp_netif_create_default_wifi_sta();
  wifi_init_config_t wc = WIFI_INIT_CONFIG_DEFAULT();
  ESP_ERROR_CHECK(esp_wifi_init(&wc));
  esp_event_handler_register(WIFI_EVENT, ESP_EVENT_ANY_ID, on_wifi, NULL);
  ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_STA));
  ESP_ERROR_CHECK(esp_wifi_start());
  // Same as the speakers (patch 0005): power save gave second-long stalls.
  esp_wifi_set_ps(WIFI_PS_NONE);
  esp_wifi_set_bandwidth(WIFI_IF_STA, WIFI_BW_HT20);

  civitone_setup_start();  // runs the setup page's web server while the setup network is up
  civitone_ctrl_start();
  capture_start();
  ptt_start();
}
