#include "es8388.h"

#include "driver/i2c_master.h"
#include "esp_log.h"

static const char *TAG = "ES8388";

#define ADDR 0x10

static i2c_master_dev_handle_t s_dev;

static esp_err_t wr(uint8_t reg, uint8_t val) {
  uint8_t b[2] = {reg, val};
  esp_err_t err = i2c_master_transmit(s_dev, b, 2, 100);
  if (err != ESP_OK) ESP_LOGE(TAG, "write reg 0x%02x: %s", reg, esp_err_to_name(err));
  return err;
}

esp_err_t es8388_init(int sda, int scl) {
  i2c_master_bus_config_t bus = {
      .i2c_port = I2C_NUM_0,
      .sda_io_num = sda,
      .scl_io_num = scl,
      .clk_source = I2C_CLK_SRC_DEFAULT,
      .glitch_ignore_cnt = 7,
      .flags.enable_internal_pullup = true,
  };
  i2c_master_bus_handle_t h;
  esp_err_t err = i2c_new_master_bus(&bus, &h);
  if (err != ESP_OK) return err;
  i2c_device_config_t dev = {.dev_addr_length = I2C_ADDR_BIT_LEN_7, .device_address = ADDR, .scl_speed_hz = 100000};
  if ((err = i2c_master_bus_add_device(h, &dev, &s_dev)) != ESP_OK) return err;
  if ((err = i2c_master_probe(h, ADDR, 100)) != ESP_OK) {
    ESP_LOGE(TAG, "no codec at 0x%02x (SDA %d, SCL %d)", ADDR, sda, scl);
    return err;
  }
  // Sequence from ESP-ADF's es8388 driver, ADC path only.
  wr(0x19, 0x04);  // DAC muted
  wr(0x01, 0x50);  // analog reference
  wr(0x02, 0x00);  // chip power: all on
  wr(0x35, 0xA0);  // low-power DAC references
  wr(0x37, 0xD0);
  wr(0x39, 0xD0);
  wr(0x08, 0x00);  // I2S slave
  wr(0x04, 0xC0);  // DAC and outputs powered down
  wr(0x00, 0x12);  // control 1: play & record mode
  wr(0x03, 0xFF);  // ADC off while configuring
  wr(0x0B, 0x02);  // ADC control 3
  wr(0x0C, 0x0C);  // 16-bit, Philips I2S
  wr(0x0D, 0x02);  // MCLK/LRCK = 256
  wr(0x10, 0x00);  // ADC digital volume L 0 dB
  wr(0x11, 0x00);  // ADC digital volume R 0 dB
  return wr(0x03, 0x00);  // ADC, inputs and mic bias on
}

esp_err_t es8388_set_input(es8388_input_t in) {
  return wr(0x0A, in == ES8388_IN_MIC ? 0x50 : 0x00);
}

esp_err_t es8388_set_gain_db(int db) {
  int step = db < 0 ? 0 : db > 24 ? 8 : db / 3;
  return wr(0x09, (uint8_t)(step << 4 | step));
}
