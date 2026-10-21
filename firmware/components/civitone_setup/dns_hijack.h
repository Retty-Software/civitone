#pragma once

#include <stdint.h>

// ap_ip in network byte order (esp_netif_ip_info_t.ip.addr).
void dns_hijack_start(uint32_t ap_ip);
void dns_hijack_stop(void);
