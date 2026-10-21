// Amped-ESP32-Plus bring-up diagnostic.
//
// 1. Drives the DAC/amp enable pin (GPIO13) high.
// 2. Scans I2C on both documented pin orders (SDA21/SCL27 and SDA27/SCL21).
// 3. Configures the PCM5122 for PLL-from-BCK (no MCLK), unmuted, 0 dB.
// 4. Plays a 440 Hz sine on I2S (BCK26, WS25, DOUT22) and prints the DAC's
//    clock and power-state registers every 2 s so we can see what it thinks.
#include <Arduino.h>
#include <Wire.h>
#include <driver/i2s.h>
#include <math.h>

static const int PIN_EN = 13;
static const int PIN_BCK = 26, PIN_WS = 25, PIN_DOUT = 22;
static const uint8_t DAC_ADDR = 0x4D;
static const int RATE = 48000;

static int sdaPin = -1, sclPin = -1;

static bool scan(int sda, int scl) {
  Wire.end();
  Wire.begin(sda, scl, 100000);
  bool found = false;
  Serial.printf("I2C scan SDA=%d SCL=%d:", sda, scl);
  for (uint8_t a = 1; a < 127; a++) {
    Wire.beginTransmission(a);
    if (Wire.endTransmission() == 0) {
      Serial.printf(" 0x%02X", a);
      if (a == DAC_ADDR) found = true;
    }
  }
  Serial.println(found ? "  <- PCM5122 found" : "");
  return found;
}

static bool wr(uint8_t reg, uint8_t val) {
  Wire.beginTransmission(DAC_ADDR);
  Wire.write(reg);
  Wire.write(val);
  uint8_t e = Wire.endTransmission();
  if (e) Serial.printf("  write reg %d failed (%d)\n", reg, e);
  return e == 0;
}

static int rd(uint8_t reg) {
  Wire.beginTransmission(DAC_ADDR);
  Wire.write(reg);
  if (Wire.endTransmission(false)) return -1;
  if (Wire.requestFrom((int)DAC_ADDR, 1) != 1) return -1;
  return Wire.read();
}

static void dump(const char *when) {
  wr(0, 0);  // page 0
  Serial.printf("[%s] pwr(2)=0x%02X mute(3)=0x%02X pllref(13)=0x%02X errdet(37)=0x%02X fmt(40)=0x%02X "
                "volL(61)=0x%02X volR(62)=0x%02X clkdet(91)=0x%02X stat(94)=0x%02X clkerr(95)=0x%02X "
                "mute_mon(108)=0x%02X state(118)=0x%02X  EN=%d\n",
                when, rd(2), rd(3), rd(13), rd(37), rd(40), rd(61), rd(62), rd(91), rd(94), rd(95),
                rd(108), rd(118), digitalRead(PIN_EN));
}

void setup() {
  Serial.begin(115200);
  delay(500);
  Serial.println("\n=== Civitone Amped-ESP32-Plus diagnostic ===");

  pinMode(PIN_EN, OUTPUT);
  digitalWrite(PIN_EN, HIGH);
  Serial.println("GPIO13 (DAC/AMP EN) = HIGH");
  delay(50);

  if (scan(21, 27)) { sdaPin = 21; sclPin = 27; }
  else if (scan(27, 21)) { sdaPin = 27; sclPin = 21; }
  else Serial.println("PCM5122 NOT FOUND on either documented pin order.");
  if (sdaPin >= 0) {
    Wire.end();
    Wire.begin(sdaPin, sclPin, 100000);
    Serial.printf("Using I2C SDA=%d SCL=%d\n", sdaPin, sclPin);
    dump("before config");
    wr(0, 0x00);   // page 0
    wr(1, 0x11);   // reset modules + registers
    delay(10);
    wr(1, 0x00);
    // Exact stock snapclient sequence (pcm51xx_reg_cfg.h)
    wr(0x03, 0x00); wr(0x2a, 0x11); wr(0x02, 0x00); wr(0x0d, 0x10);
    wr(0x25, 0x08); wr(0x3d, 0x30); wr(0x3e, 0x30);
    dump("after config");
  }

  i2s_config_t cfg = {};
  cfg.mode = (i2s_mode_t)(I2S_MODE_MASTER | I2S_MODE_TX);
  cfg.sample_rate = RATE;
  cfg.bits_per_sample = I2S_BITS_PER_SAMPLE_16BIT;
  cfg.channel_format = I2S_CHANNEL_FMT_RIGHT_LEFT;
  cfg.communication_format = I2S_COMM_FORMAT_STAND_I2S;
  cfg.dma_buf_count = 8;
  cfg.dma_buf_len = 256;
  cfg.use_apll = true;
  i2s_pin_config_t pins = {};
  pins.mck_io_num = GPIO_NUM_0;  // stock config outputs MCLK on GPIO0
  pins.bck_io_num = PIN_BCK;
  pins.ws_io_num = PIN_WS;
  pins.data_out_num = PIN_DOUT;
  pins.data_in_num = I2S_PIN_NO_CHANGE;
  i2s_driver_install(I2S_NUM_0, &cfg, 0, nullptr);
  i2s_set_pin(I2S_NUM_0, &pins);
  Serial.println(">>> PHASE A: stock register sequence + MCLK on GPIO0, tone 440 Hz");
  Serial.println("I2S started: BCK=26 WS=25 DOUT=22, 48 kHz 16-bit. Playing 440 Hz at -20 dBFS.");
}

void loop() {
  static float phase = 0;
  static int16_t buf[512];
  static uint32_t lastDump = 0;
  static bool phaseB = false;
  if (!phaseB && millis() > 12000) {
    phaseB = true;
    Serial.println(">>> PHASE B: reg 37 = 0x7D (ignore all clock-detect errors), tone 660 Hz");
    wr(2, 0x10); wr(37, 0x7D); wr(2, 0x00);
  }
  const float step = 2 * M_PI * (phaseB ? 660.0f : 440.0f) / RATE;
  const float amp = 32767 * 0.1f;  // -20 dBFS
  for (int i = 0; i < 512; i += 2) {
    int16_t s = (int16_t)(sinf(phase) * amp);
    buf[i] = s;
    buf[i + 1] = s;
    phase += step;
    if (phase > 2 * M_PI) phase -= 2 * M_PI;
  }
  size_t written;
  i2s_write(I2S_NUM_0, buf, sizeof(buf), &written, portMAX_DELAY);
  if (sdaPin >= 0 && millis() - lastDump > 2000) {
    lastDump = millis();
    dump("playing");
  }
}
