#include "settings_manager.h"

esp_err_t settings_set_mdns_enabled(bool enabled) { return ESP_OK; }
esp_err_t settings_get_server_host(char *host, size_t max_len) { return ESP_ERR_NOT_FOUND; }
esp_err_t settings_set_server_host(const char *host) { return ESP_OK; }
esp_err_t settings_clear_server_host(void) { return ESP_OK; }
esp_err_t settings_set_server_port(int32_t port) { return ESP_OK; }
esp_err_t settings_clear_server_port(void) { return ESP_OK; }
