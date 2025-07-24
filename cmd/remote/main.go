package main

import (
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/wizzomafizzo/mrext/cmd/remote/claude"
	"github.com/wizzomafizzo/mrext/cmd/remote/control"
	"github.com/wizzomafizzo/mrext/cmd/remote/games"
	"github.com/wizzomafizzo/mrext/cmd/remote/menu"
	"github.com/wizzomafizzo/mrext/cmd/remote/music"
	"github.com/wizzomafizzo/mrext/cmd/remote/screenshots"
	"github.com/wizzomafizzo/mrext/cmd/remote/scripts"
	"github.com/wizzomafizzo/mrext/cmd/remote/settings"
	"github.com/wizzomafizzo/mrext/cmd/remote/systems"
	"github.com/wizzomafizzo/mrext/cmd/remote/wallpapers"
	"github.com/wizzomafizzo/mrext/cmd/remote/websocket"
	"github.com/wizzomafizzo/mrext/pkg/input"
	"github.com/wizzomafizzo/mrext/pkg/mister"
	"github.com/wizzomafizzo/mrext/pkg/tracker"

	gc "github.com/rthornton128/goncurses"

	"github.com/gorilla/mux"
	"github.com/rs/cors"
	"github.com/wizzomafizzo/mrext/pkg/config"
	"github.com/wizzomafizzo/mrext/pkg/curses"
	"github.com/wizzomafizzo/mrext/pkg/service"
)

const (
	appVersion = "0.4"
	appName    = "remote"
	appPort    = 8182
)

var logger = service.NewLogger(appName)

//go:embed _client
var client embed.FS

func wsConnectPayload(trk *tracker.Tracker) func() []string {
	return func() []string {
		response := []string{
			games.GetIndexingStatus(),
		}

		if trk != nil {
			response = append(response, "coreRunning:"+trk.ActiveCore)
			response = append(response, "gameRunning:"+trk.ActiveGame)
		}

		return response
	}
}

func wsMsgHandler(kbd input.Keyboard) func(string) string {
	return func(msg string) string {
		parts := strings.SplitN(msg, ":", 2)
		cmd := parts[0]
		args := ""
		if len(parts) > 1 {
			args = parts[1]
		}

		switch cmd {
		case "getIndexStatus":
			return games.GetIndexingStatus()
		case "kbd":
			err := control.SendKeyboard(kbd, args)
			if err != nil {
				return "invalid"
			}
			return ""
		case "kbdRaw":
			code, err := strconv.Atoi(args)
			if err != nil {
				return "invalid"
			}

			err = control.SendRawKeyboard(kbd, code)
			if err != nil {
				return "invalid"
			}

			return ""
		case "kbdRawDown":
			code, err := strconv.Atoi(args)
			if err != nil {
				return "invalid"
			}

			err = control.SendRawKeyboardDown(kbd, code)
			if err != nil {
				return "invalid"
			}

			return ""
		case "kbdRawUp":
			code, err := strconv.Atoi(args)
			if err != nil {
				return "invalid"
			}

			err = control.SendRawKeyboardUp(kbd, code)
			if err != nil {
				return "invalid"
			}

			return ""
		default:
			return "invalid"
		}
	}
}

func startService(logger *service.Logger, cfg *config.UserConfig) (func() error, error) {
	kbd, err := input.NewKeyboard()
	if err != nil {
		logger.Error("failed to initialize keyboard: %s", err)
		return nil, err
	}

	trk, stopTracker, err := games.StartTracker(logger, cfg)
	if err != nil {
		logger.Error("failed to start tracker: %s", err)
		return nil, err
	}

	runStartupTasks(logger, cfg, trk)

	var stopMdns func() error
	if cfg.Remote.MdnsService {
		go func() {
			stopMdns = mister.TryStartMdns(logger, appVersion)
		}()
	}

	router := mux.NewRouter()
	setupApi(router.PathPrefix("/api").Subrouter(), kbd, trk, logger, cfg)
	router.PathPrefix("/").Handler(http.HandlerFunc(appHandler))

	corsHandler := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"GET", "POST", "DELETE", "PUT"},
	})

	srv := &http.Server{
		Handler: corsHandler.Handler(router),
		Addr:    ":" + fmt.Sprint(appPort),
		// TODO: this will not work for large file uploads
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("critical server error: %s", err)
			os.Exit(1)
		}
	}()

	return func() error {
		kbd.Close()

		if stopMdns != nil {
			err := stopMdns()
			if err != nil {
				logger.Error("failed to stop mdns: %s", err)
			}
		}

		err := stopTracker()
		if err != nil {
			logger.Error("failed to stop tracker: %s", err)
		}

		err = srv.Close()
		if err != nil {
			logger.Error("failed to shutdown server: %s", err)
		}

		return nil
	}, nil
}

func setupApi(sub *mux.Router, kbd input.Keyboard, trk *tracker.Tracker, logger *service.Logger, cfg *config.UserConfig) {
	sub.HandleFunc("/ws", websocket.Handle(logger, wsConnectPayload(trk), wsMsgHandler(kbd)))

	sub.HandleFunc("/screenshots", screenshots.AllScreenshots(logger)).Methods("GET")
	sub.HandleFunc("/screenshots", screenshots.TakeScreenshot(logger)).Methods("POST")
	sub.HandleFunc("/screenshots/{core}/{image}", screenshots.ViewScreenshot(logger)).Methods("GET")
	sub.HandleFunc("/screenshots/{core}/{image}", screenshots.DeleteScreenshot(logger)).Methods("DELETE")

	sub.HandleFunc("/systems", systems.ListSystems(logger)).Methods("GET")
	sub.HandleFunc("/systems/{id}", systems.LaunchCore(cfg, logger)).Methods("POST")

	sub.HandleFunc("/wallpapers", wallpapers.AllWallpapersHandler(logger)).Methods("GET")
	sub.HandleFunc("/wallpapers", wallpapers.UnsetWallpaperHandler(logger)).Methods("DELETE")
	sub.HandleFunc("/wallpapers/{filename:.*}", wallpapers.ViewWallpaperHandler(logger)).Methods("GET")
	sub.HandleFunc("/wallpapers/{filename:.*}", wallpapers.SetWallpaperHandler(logger)).Methods("POST")

	sub.HandleFunc("/music/status", music.Status(logger)).Methods("GET")
	sub.HandleFunc("/music/play", music.Play(logger)).Methods("POST")
	sub.HandleFunc("/music/stop", music.Stop(logger)).Methods("POST")
	sub.HandleFunc("/music/next", music.Skip(logger)).Methods("POST")
	sub.HandleFunc("/music/playback/{playback}", music.SetPlayback(logger)).Methods("POST")
	sub.HandleFunc("/music/playlist", music.AllPlaylists(logger)).Methods("GET")
	sub.HandleFunc("/music/playlist/{playlist}", music.SetPlaylist(logger)).Methods("POST")

	sub.HandleFunc("/games/search", games.Search(logger)).Methods("POST")
	sub.HandleFunc("/games/search/systems", games.ListSystems(logger)).Methods("GET")
	sub.HandleFunc("/games/launch", games.LaunchGame(logger, cfg)).Methods("POST")
	sub.HandleFunc("/games/index", games.GenerateSearchIndex(logger, cfg)).Methods("POST")
	sub.HandleFunc("/games/playing", games.HandlePlaying(trk)).Methods("GET")
	sub.HandleFunc("/games/view", games.ListGamesFolder(logger)).Methods("POST")

	sub.HandleFunc("/l/{data:.*}", games.LaunchToken(logger, cfg, kbd)).Methods("GET")

	sub.HandleFunc("/launch", games.LaunchFile(logger, cfg)).Methods("POST")
	sub.HandleFunc("/launch/menu", games.LaunchMenu).Methods("POST")
	sub.HandleFunc("/launch/new", games.CreateLauncher(logger, cfg)).Methods("POST")

	sub.HandleFunc("/controls/keyboard/{key}", control.HandleKeyboard(kbd)).Methods("POST")
	sub.HandleFunc("/controls/keyboard-raw/{key}", control.HandleRawKeyboard(kbd, logger)).Methods("POST")

	sub.HandleFunc("/menu/view", menu.ListFolder(logger)).Methods("POST")
	sub.HandleFunc("/menu/files/create", menu.HandleCreateFile(logger)).Methods("POST")
	sub.HandleFunc("/menu/files/rename", menu.HandleRenameFile(logger)).Methods("POST")
	sub.HandleFunc("/menu/files/delete", menu.HandleDeleteFile(logger)).Methods("POST")

	sub.HandleFunc("/scripts/launch/{filename}", scripts.HandleLaunchScript(logger, kbd)).Methods("POST")
	sub.HandleFunc("/scripts/list", scripts.HandleListScripts(logger)).Methods("GET")
	sub.HandleFunc("/scripts/console", scripts.HandleOpenScriptsConsole(logger, kbd)).Methods("POST")
	sub.HandleFunc("/scripts/kill", scripts.HandleKillActiveScript(logger)).Methods("POST")

	sub.HandleFunc("/settings/inis", settings.HandleListInis(logger)).Methods("GET")
	sub.HandleFunc("/settings/inis", settings.HandleSetActiveIni(logger)).Methods("PUT")
	sub.HandleFunc("/settings/inis/1", settings.HandleLoadIni(logger, 1)).Methods("GET")
	sub.HandleFunc("/settings/inis/1", settings.HandleSaveIni(logger, 1)).Methods("PUT")
	sub.HandleFunc("/settings/inis/2", settings.HandleLoadIni(logger, 2)).Methods("GET")
	sub.HandleFunc("/settings/inis/2", settings.HandleSaveIni(logger, 2)).Methods("PUT")
	sub.HandleFunc("/settings/inis/3", settings.HandleLoadIni(logger, 3)).Methods("GET")
	sub.HandleFunc("/settings/inis/3", settings.HandleSaveIni(logger, 3)).Methods("PUT")
	sub.HandleFunc("/settings/inis/4", settings.HandleLoadIni(logger, 4)).Methods("GET")
	sub.HandleFunc("/settings/inis/4", settings.HandleSaveIni(logger, 4)).Methods("PUT")

	sub.HandleFunc("/settings/cores/menu", settings.HandleSetMenuBackgroundMode(logger)).Methods("PUT")
	sub.HandleFunc("/settings/remote/restart", settings.HandleRestartRemote(logger, cfg)).Methods("POST")
	sub.HandleFunc("/settings/remote/log", settings.HandleDownloadRemoteLog(logger)).Methods("GET")
	sub.HandleFunc("/settings/remote/peers", settings.HandleListPeers(logger)).Methods("GET")
	sub.HandleFunc("/settings/remote/logo", settings.HandleLogoFile(logger, client, cfg)).Methods("GET")
	sub.HandleFunc("/settings/system/reboot", settings.HandleReboot(logger)).Methods("POST")
	sub.HandleFunc("/settings/system/generate-mac", settings.HandleGenerateMac(logger)).Methods("GET")
	// ✅ NEW: Frontend configuration endpoint
	sub.HandleFunc("/settings/frontend/config", settings.HandleFrontendConfig(logger, cfg)).Methods("GET")

	sub.HandleFunc("/nfc/status", games.NfcStatus(logger)).Methods("GET")
	sub.HandleFunc("/nfc/write", games.NfcWrite(logger)).Methods("POST")
	sub.HandleFunc("/nfc/cancel", games.NfcCancel(logger)).Methods("POST")

	sub.HandleFunc("/sysinfo", settings.HandleSystemInfo(logger, cfg, appVersion)).Methods("GET")

	// Claude AI endpoints
	sub.HandleFunc("/claude/status", claude.HandleStatus(logger, cfg)).Methods("GET")
	sub.HandleFunc("/claude/chat", claude.HandleChat(logger, cfg, trk)).Methods("POST")
	sub.HandleFunc("/claude/suggestions", claude.HandleSuggestions(logger, cfg, trk)).Methods("GET")
	sub.HandleFunc("/claude/playlist", claude.HandlePlaylist(logger, cfg, trk)).Methods("POST")
	sub.HandleFunc("/claude/config", claude.HandleUpdateConfig(logger, cfg)).Methods("PUT")
	sub.HandleFunc("/claude/export", claude.HandleExportPlaylist(logger)).Methods("POST")
	sub.HandleFunc("/claude/cache/clear", claude.HandleClearCache(logger)).Methods("POST")
	sub.HandleFunc("/api/claude/active-game-suggestion", claude.HandleActiveGameSuggestion(logger, cfg, trk)).Methods("GET")
	sub.HandleFunc("/claude/debug-active-game", claude.HandleDebugActiveGame(logger, cfg, trk)).Methods("GET")
	sub.HandleFunc("/claude/game-context", claude.HandleGetGameContext(logger, cfg, trk)).Methods("GET")
}

func appHandler(rw http.ResponseWriter, req *http.Request) {
	if !strings.HasPrefix(req.URL.Path, "/") {
		req.URL.Path = "/" + req.URL.Path
	}

	build, err := fs.Sub(client, "_client/build")
	if err != nil {
		logger.Error("could not create client sub fs: %s", err)
		return
	}

	filePath := strings.TrimLeft(path.Clean(req.URL.Path), "/")
	if _, err := build.Open(filePath); err != nil {
		req.URL.Path = "/"
	}

	http.FileServer(http.FS(build)).ServeHTTP(rw, req)
}

func main() {
	svcOpt := flag.String("service", "", "manage playlog service (start, stop, restart, status)")
	uninstallOpt := flag.Bool("uninstall", false, "uninstall MiSTer Remote")
	flag.Parse()

	cfg, err := config.LoadUserConfig(appName, &config.UserConfig{
		Remote: config.GetDefaultRemoteConfig(), // ✅ USE default configuration
		Claude: config.GetDefaultClaudeConfig(), // Existing
	})
	if err != nil {
		logger.Error("error loading user config: %s", err)
		fmt.Println("Error loading config file:", err)
		os.Exit(1)
	}

	err = os.MkdirAll(config.MrextConfigFolder, 0755)
	if err != nil {
		logger.Error("error creating config folder: %s", err)
		fmt.Println("Error creating config folder:", err)
		os.Exit(1)
	}

	svc, err := service.NewService(service.ServiceArgs{
		Name:   appName,
		Logger: logger,
		Entry: func() (func() error, error) {
			return startService(logger, cfg)
		},
	})
	if err != nil {
		logger.Error("creating service: %s", err)
		fmt.Println("Error creating service:", err)
		os.Exit(1)
	}

	if *uninstallOpt {
		uninstallService(svc)
		os.Exit(0)
	}

	svc.ServiceHandler(svcOpt)

	if !svc.Running() {
		err := svc.Start()
		if err != nil {
			logger.Error("starting service: %s", err)
			fmt.Println("Error starting service:", err)
			os.Exit(1)
		}
	}

	interactive := true

	stdscr, err := curses.Setup()
	if err != nil {
		logger.Error("starting curses: %s", err)
		interactive = false
	}
	defer gc.End()

	if interactive {
		err = tryAddStartup(stdscr)
		if err != nil {
			gc.End()
			logger.Error("adding startup: %s", err)

			if errors.As(err, &curses.SetupWindowError{}) {
				interactive = false
			} else {
				fmt.Println("Error adding to startup:", err)
			}
		}
	}

	if interactive {
		action, err := displayServiceInfo(stdscr, svc, cfg)
		if err != nil {
			gc.End()
			logger.Error("displaying service info: %s", err)

			if errors.As(err, &curses.SetupWindowError{}) {
				interactive = false
			} else {
				fmt.Println("Error displaying service info:", err)
			}
		} else if action == displayUninstall {
			gc.End()
			uninstallService(svc)
			os.Exit(0)
		}
	}

	if !interactive {
		tryNonInteractiveAddToStartup(true)
		displayNonInteractiveServiceInfo(svc)
	}
}
