idf_component_register(SRCS "civitone_ctrl.c"
                       INCLUDE_DIRS "include"
                       PRIV_REQUIRES esp_wifi esp_netif esp_timer nvs_flash json settings_manager mbedtls esp_hw_support esp_http_client app_update)
