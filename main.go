package main

import (
	"embed"
	"log"

	"cursorforge/internal/bridge"

	// Side-effect imports: each gen package's init() registers its message
	// types in the global proto registry so that protocodec can look up
	// response messages by Connect URL path.
	_ "cursorforge/internal/protocodec/gen/agent/v1"
	_ "cursorforge/internal/protocodec/gen/aiserver/v1"
	_ "cursorforge/internal/protocodec/gen/anyrun/v1"
	_ "cursorforge/internal/protocodec/gen/internapi/v1"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/appicon.png
var trayIcon []byte

func init() {
	application.RegisterEvent[bool]("proxyState")
}

func main() {
	proxySvc, err := bridge.NewProxyService()
	if err != nil {
		log.Fatalf("failed to init proxy service: %v", err)
	}

	app := application.New(application.Options{
		Name:        "CursorForge",
		Description: "CursorForge — Local MITM proxy & BYOK gateway for Cursor IDE",
		Services: []application.Service{
			application.NewService(proxySvc),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
	})

	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:         "CursorForge",
		Width:         1000,
		Height:        540,
		DisableResize: true,
		Frameless:     true,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 50,
			Backdrop:                application.MacBackdropTranslucent,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
		BackgroundColour: application.NewRGB(9, 9, 11),
		URL:              "/",
	})

	// Clicking the window close button hides to tray instead of quitting.
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		window.Hide()
		e.Cancel()
	})

	// ---------------- System tray ----------------
	tray := app.SystemTray.New()
	tray.SetIcon(trayIcon)
	tray.SetTooltip("CursorForge — stopped")

	menu := app.NewMenu()

	menu.Add("Show CursorForge").OnClick(func(_ *application.Context) {
		window.Show()
		window.Focus()
	})
	menu.AddSeparator()

	startItem := menu.Add("Start service")
	stopItem := menu.Add("Stop service")

	startItem.OnClick(func(_ *application.Context) {
		go func() {
			if _, err := proxySvc.StartProxy(); err != nil {
				app.Dialog.Warning().
					SetTitle("CursorForge").
					SetMessage("Failed to start:\n" + err.Error()).
					Show()
			}
		}()
	})
	stopItem.OnClick(func(_ *application.Context) {
		go func() {
			if _, err := proxySvc.StopProxy(); err != nil {
				app.Dialog.Warning().
					SetTitle("CursorForge").
					SetMessage("Failed to stop:\n" + err.Error()).
					Show()
			}
		}()
	})

	menu.AddSeparator()
	menu.Add("Open settings folder").OnClick(func(_ *application.Context) {
		_ = proxySvc.OpenSettingsFolder()
	})
	menu.AddSeparator()
	menu.Add("Quit").OnClick(func(_ *application.Context) {
		proxySvc.Shutdown()
		app.Quit()
	})

	tray.SetMenu(menu)

	// Left-click the tray icon brings the window to front.
	tray.OnClick(func() {
		window.Show()
		window.Focus()
	})

	applyTrayState := func(running bool) {
		if running {
			tray.SetTooltip("CursorForge — running on 127.0.0.1:18080")
		} else {
			tray.SetTooltip("CursorForge — stopped")
		}
		startItem.SetEnabled(!running)
		stopItem.SetEnabled(running)
	}
	applyTrayState(false)

	proxySvc.SetStateCallback(func(running bool) {
		applyTrayState(running)
		app.Event.Emit("proxyState", running)
	})

	if err := app.Run(); err != nil {
		proxySvc.Shutdown()
		log.Fatal(err)
	}
	proxySvc.Shutdown()
}
