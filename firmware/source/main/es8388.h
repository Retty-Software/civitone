// ES8388 codec on the ESP32-A1S Audio Kit, ADC only: I2S slave, 16-bit
// Philips, MCLK = 256 fs. The DAC stays powered down.
#pragma once

#include "esp_err.h"

// On the Audio Kit v2.2 A618 the onboard mics are wired to LIN2/RIN2, the
// reverse of ESP-ADF's board file (measured Oct 21, 2026: speech reached
// -12 dBFS on input 2 at 0 dB gain, nothing above the noise on input 1 at
// +24 dB).
typedef enum {
  ES8388_IN_MIC,   // LIN2/RIN2: the two onboard microphones
  ES8388_IN_LINE,  // LIN1/RIN1: the LINE IN (aux) jack
} es8388_input_t;

esp_err_t es8388_init(int sda, int scl);
esp_err_t es8388_set_input(es8388_input_t in);
// Analog PGA gain, 0..24 dB in 3 dB steps (rounded down).
esp_err_t es8388_set_gain_db(int db);
