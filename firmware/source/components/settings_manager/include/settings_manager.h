// Stand-in for snapclient's settings_manager, which civitone_ctrl uses for
// the snapserver address. A source doesn't play from snapserver, so these
// do nothing.
#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

esp_err_t settings_set_mdns_enabled(bool enabled);
esp_err_t settings_get_server_host(char *host, size_t max_len);
esp_err_t settings_set_server_host(const char *host);
esp_err_t settings_clear_server_host(void);
esp_err_t settings_set_server_port(int32_t port);
esp_err_t settings_clear_server_port(void);
