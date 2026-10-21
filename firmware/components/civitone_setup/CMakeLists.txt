idf_component_register(SRCS "civitone_setup.c" "dns_hijack.c"
                       INCLUDE_DIRS "include"
                       PRIV_REQUIRES esp_wifi esp_netif esp_http_server esp_timer nvs_flash json lwip civitone_ctrl
                       EMBED_TXTFILES "setup.html")
