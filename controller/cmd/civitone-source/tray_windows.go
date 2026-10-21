//go:build windows

package main

import (
	"context"
	_ "embed"
	"time"

	"fyne.io/systray"

	"github.com/Retty-Software/civitone/controller/internal/srcapp"
)

//go:embed icon.ico
var iconICO []byte

// runUI shows the tray icon until Quit or ctx ends.
func runUI(ctx context.Context, url string, app *srcapp.App, quit func()) {
	systray.Run(func() {
		systray.SetIcon(iconICO)
		systray.SetTooltip("Civitone Source")
		open := systray.AddMenuItem("Open Civitone Source", "Open the setup page")
		systray.AddSeparator()
		var lines []*systray.MenuItem
		systray.AddSeparator()
		exit := systray.AddMenuItem("Quit", "Stop sending audio and exit")

		refresh := func() {
			sum := app.Summary()
			if len(sum) == 0 {
				sum = []string{"No sources yet"}
			}
			for len(lines) < len(sum) {
				it := systray.AddMenuItem("", "")
				it.Disable()
				lines = append(lines, it)
			}
			for i, it := range lines {
				if i < len(sum) {
					it.SetTitle(sum[i])
					it.Show()
				} else {
					it.Hide()
				}
			}
			systray.SetTooltip("Civitone Source\n" + joinLines(sum))
		}
		refresh()
		go func() {
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					systray.Quit()
					return
				case <-open.ClickedCh:
					openBrowser(url)
				case <-exit.ClickedCh:
					quit()
					systray.Quit()
					return
				case <-t.C:
					refresh()
				}
			}
		}()
	}, func() {})
}

func joinLines(s []string) string {
	out := ""
	for i, l := range s {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}
