// Civitone node control channel: outbound WebSocket to the controller,
// hello + heartbeat every 5 s + commands. See docs/control-protocol.md.
#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

struct cJSON;

// Starts the control task. Safe to call before WiFi has an address.
void civitone_ctrl_start(void);

// Called by the DAC task whenever volume or mute changes, for the heartbeat.
void civitone_ctrl_note_dac(int volume, bool muted);

// Setup saved a new controller URL / adopt code: reconnect with them.
void civitone_ctrl_reload(void);

// This firmware's version, and the name the controller knows the node by
// ("" until it has been welcomed as adopted once).
const char *civitone_ctrl_fw_version(void);
const char *civitone_ctrl_name(void);

// "civ-xxxx" (speaker) or "src-xxxx" (source), from the MAC.
const char *civitone_ctrl_device_id(void);

// Sources: one binary frame of 48 kHz mono s16le PCM. False (and dropped)
// unless connected and adopted. Never blocks for long.
bool civitone_ctrl_send_audio(const void *pcm, size_t len);

// Sources: one text frame (a JSON object), e.g. {"t":"ptt","down":true}.
// False unless connected and adopted.
bool civitone_ctrl_send_text(const char *json);

// Nonzero while connected and adopted; changes on every new adopted
// session, so state the controller must know can be re-sent.
uint32_t civitone_ctrl_session(void);

// Optional, defined by the board: commands the control channel doesn't know
// (return true if handled, *err NULL on success) and extra heartbeat fields.
bool civitone_board_command(const char *cmd, const struct cJSON *msg, const char **err);
void civitone_board_heartbeat(struct cJSON *hb);

#ifdef __cplusplus
}
#endif
