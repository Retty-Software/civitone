// Captive-portal DNS: while the setup network is up, every A query from a
// client on it gets the node's AP address, so phones pop up the setup page.

#include "dns_hijack.h"

#include <stdbool.h>
#include <string.h>

#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "lwip/sockets.h"

static const char *TAG = "CIVI_DNS";
static TaskHandle_t s_task;
static volatile bool s_stop;
static uint32_t s_answer_ip;  // network byte order

static void dns_task(void *arg) {
  int sock = socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
  struct sockaddr_in addr = {
      .sin_family = AF_INET,
      .sin_port = htons(53),
      .sin_addr.s_addr = s_answer_ip,  // AP side only, never the STA side
  };
  struct timeval tv = {.tv_sec = 1};
  setsockopt(sock, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv);
  if (sock < 0 || bind(sock, (struct sockaddr *)&addr, sizeof addr) != 0) {
    ESP_LOGE(TAG, "bind :53 failed");
    if (sock >= 0) close(sock);
    s_task = NULL;
    vTaskDelete(NULL);
    return;
  }

  uint8_t buf[512];
  while (!s_stop) {
    struct sockaddr_in from;
    socklen_t flen = sizeof from;
    int n = recvfrom(sock, buf, sizeof buf - 16, 0, (struct sockaddr *)&from, &flen);
    if (n < 12) continue;
    // Find the end of the first question: QNAME labels, then QTYPE, QCLASS.
    int p = 12;
    while (p < n && buf[p] != 0) p += buf[p] + 1;
    p += 1;
    if (p + 4 > n) continue;
    uint16_t qtype = (buf[p] << 8) | buf[p + 1];
    p += 4;

    buf[2] = 0x81;  // response, recursion desired
    buf[3] = 0x80;  // recursion available, no error
    buf[6] = 0;     // ANCOUNT
    buf[7] = qtype == 1 ? 1 : 0;
    buf[8] = buf[9] = buf[10] = buf[11] = 0;  // NSCOUNT, ARCOUNT
    int len = p;  // drop anything after the first question
    if (qtype == 1) {
      const uint8_t ans[] = {0xC0, 0x0C, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4};
      memcpy(buf + len, ans, sizeof ans);
      len += sizeof ans;
      memcpy(buf + len, &s_answer_ip, 4);
      len += 4;
    }
    sendto(sock, buf, len, 0, (struct sockaddr *)&from, flen);
  }
  close(sock);
  s_task = NULL;
  vTaskDelete(NULL);
}

void dns_hijack_start(uint32_t ap_ip) {
  if (s_task) return;
  s_answer_ip = ap_ip;
  s_stop = false;
  xTaskCreate(dns_task, "civi_dns", 3072, NULL, 4, &s_task);
}

void dns_hijack_stop(void) { s_stop = true; }
