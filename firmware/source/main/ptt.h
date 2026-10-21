// Push-to-talk for a paging mic: KEY1 on the board, or a mic's talk switch
// wired across it. The controller decides whether this source pages.
#pragma once

#include <stdbool.h>

void ptt_start(void);
bool ptt_down(void);
