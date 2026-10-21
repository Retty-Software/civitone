idf_component_register(SRCS "main.c" "es8388.c" "capture.c" "ptt.c"
                       INCLUDE_DIRS "."
                       PRIV_REQUIRES esp_driver_i2s esp_driver_i2c esp_wifi esp_netif esp_event esp_http_server
                                     esp_ringbuf esp_driver_gpio esp_timer nvs_flash json civitone_ctrl civitone_setup)
