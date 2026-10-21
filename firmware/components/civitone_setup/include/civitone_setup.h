#pragma once

#include <stdbool.h>

// Setup mode: an open "civitone-xxxx" network with a captive page where the
// owner picks a Wi-Fi network, enters the server address and an adopt code.
// See docs/node-setup.md.

// Call once after network_if_init().
void civitone_setup_start(void);

// Patched into the Wi-Fi driver's disconnect handler: false while setup is
// scanning or switching networks, so the driver doesn't fight it.
bool civitone_setup_should_reconnect(void);

// Control channel progress, shown on the setup page.
typedef enum {
  CIVI_SRV_IDLE,       // not configured, or waiting for Wi-Fi
  CIVI_SRV_CONNECTING, // dialling the controller
  CIVI_SRV_PENDING,    // controller accepted the adopt code; waiting for approval
  CIVI_SRV_ADOPTED,    // connected with a device token
  CIVI_SRV_REJECTED,   // code wrong/expired, rejected, or token unknown
  CIVI_SRV_ERROR,      // could not reach the controller
} civi_srv_state_t;

void civitone_setup_note_server(civi_srv_state_t state, const char *detail);
