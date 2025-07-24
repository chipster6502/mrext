// pkg/tracker/sam.go
package tracker

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/wizzomafizzo/mrext/pkg/service"
)

const (
	SAMGameFile = "/tmp/SAM_Game.txt"
	SAMPidFile  = "/tmp/.SAM_tmp/SAM.pid"
)

// SAMGameInfo represents the parsed content from SAM_Game.txt
type SAMGameInfo struct {
	GameName   string
	SystemName string
	IsActive   bool
	LastUpdate time.Time
}

// SAMWatcher monitors SAM activity and game changes
type SAMWatcher struct {
	logger      *service.Logger
	tracker     *Tracker
	watcher     *fsnotify.Watcher
	currentGame *SAMGameInfo
	stopChan    chan bool
}

// NewSAMWatcher creates a new SAM watcher instance
func NewSAMWatcher(logger *service.Logger, tr *Tracker) (*SAMWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	samWatcher := &SAMWatcher{
		logger:   logger,
		tracker:  tr,
		watcher:  watcher,
		stopChan: make(chan bool),
	}

	return samWatcher, nil
}

// Start begins monitoring SAM activity with enhanced game change detection
func (sw *SAMWatcher) Start() error {
	// Add SAM game file directory to watcher
	err := sw.watcher.Add("/tmp/")
	if err != nil {
		sw.logger.Error("sam watcher: failed to watch /tmp/: %s", err)
		return err
	}

	// Start the monitoring goroutine
	go sw.monitor()

	// Initial check for existing SAM game
	sw.logger.Info("sam watcher: performing initial SAM check...")
	if sw.IsSAMActive() {
		sw.checkSAMGameChange()
		sw.logger.Info("sam watcher: SAM is active, monitoring for changes")
	} else {
		sw.logger.Info("sam watcher: SAM is not active, will monitor for activation")
	}

	sw.logger.Info("sam watcher: 🚀 started monitoring SAM activity and game changes")
	return nil
}

// Stop stops the SAM watcher
func (sw *SAMWatcher) Stop() error {
	sw.stopChan <- true
	return sw.watcher.Close()
}

// monitor runs the file watching loop with enhanced game change detection
func (sw *SAMWatcher) monitor() {
	// Start a ticker for periodic SAM checks (every 5 seconds)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case event, ok := <-sw.watcher.Events:
			if !ok {
				return
			}

			// Check if SAM_Game.txt was modified
			if strings.Contains(event.Name, "SAM_Game.txt") {
				if event.Op&fsnotify.Write == fsnotify.Write {
					sw.logger.Info("sam watcher: 🎮 SAM_Game.txt WRITE detected - potential game change")
					// Small delay to ensure file write is complete
					time.Sleep(100 * time.Millisecond)
					sw.checkSAMGameChange()
				} else if event.Op&fsnotify.Create == fsnotify.Create {
					sw.logger.Info("sam watcher: 🎮 SAM_Game.txt CREATED - SAM likely started")
					time.Sleep(100 * time.Millisecond)
					sw.checkSAMGameChange()
				} else if event.Op&fsnotify.Remove == fsnotify.Remove {
					sw.logger.Info("sam watcher: ❌ SAM_Game.txt REMOVED - SAM likely stopped")
					sw.handleSAMStop()
				}
			}

		case err, ok := <-sw.watcher.Errors:
			if !ok {
				return
			}
			sw.logger.Error("sam watcher: file watch error: %s", err)

		case <-ticker.C:
			// Periodic check in case we missed a file event
			sw.periodicSAMCheck()

		case <-sw.stopChan:
			return
		}
	}
}

// IsSAMActive checks if SAM is currently active using multiple detection methods
func (sw *SAMWatcher) IsSAMActive() bool {
	// Method 1: Check for tmux session named "SAM" (most reliable)
	cmd := exec.Command("tmux", "has-session", "-t", "SAM")
	if err := cmd.Run(); err == nil {
		sw.logger.Debug("sam watcher: SAM active - tmux session found")
		return true
	}

	// Method 2: Check for SAM processes
	if sw.isSAMProcessRunning() {
		sw.logger.Debug("sam watcher: SAM active - process found")
		return true
	}

	// Method 3: Check SAM_Game.txt modification time (recent activity)
	if info, err := os.Stat(SAMGameFile); err == nil {
		// Consider SAM active if file was modified in last 30 seconds
		if time.Since(info.ModTime()) < 30*time.Second {
			sw.logger.Debug("sam watcher: SAM active - recent file modification")
			return true
		}
	}

	// Method 4: Check SAM temp directory for active files
	if sw.checkSAMTempFiles() {
		sw.logger.Debug("sam watcher: SAM active - temp files found")
		return true
	}

	return false
}

// isSAMProcessRunning checks for running SAM processes
func (sw *SAMWatcher) isSAMProcessRunning() bool {
	samProcesses := []string{
		"MiSTer_SAM_on.sh loop_core",
		"MiSTer_SAM_on.sh initial_start",
		"MiSTer_SAM_MCP",
	}

	for _, process := range samProcesses {
		cmd := exec.Command("pgrep", "-f", process)
		if err := cmd.Run(); err == nil {
			return true
		}
	}
	return false
}

// checkSAMTempFiles checks for SAM temporary files indicating active session
func (sw *SAMWatcher) checkSAMTempFiles() bool {
	tempFiles := []string{
		"/tmp/.SAM_tmp/SAM.pid",
		"/tmp/.SAM_tmp/samvideo_init",
		"/tmp/SAM_Games.log",
		"/tmp/SAM_Game.mgl", // Also check for MGL file
	}

	for _, file := range tempFiles {
		if info, err := os.Stat(file); err == nil {
			// Check if file is recent (within last 2 minutes)
			if time.Since(info.ModTime()) < 2*time.Minute {
				sw.logger.Debug("sam watcher: found active SAM temp file: %s", file)
				return true
			}
		}
	}
	return false
}

// parseSAMGameFile parses the content of SAM_Game.txt
// SAM writes in the format: "Game Name (core_name)" - e.g. "PC Genjin 2 (tgfx16)"
func (sw *SAMWatcher) parseSAMGameFile() (*SAMGameInfo, error) {
	file, err := os.Open(SAMGameFile)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return nil, scanner.Err()
	}

	content := strings.TrimSpace(scanner.Text())
	if content == "" {
		return nil, nil
	}

	sw.logger.Debug("sam watcher: parsing SAM_Game.txt content: '%s'", content)

	// Get file modification time
	fileInfo, err := os.Stat(SAMGameFile)
	if err != nil {
		sw.logger.Warn("sam watcher: couldn't get file info: %s", err)
	}

	// Parse SAM format: "Game Name (core_name)"
	// The core name is the last parentheses content
	re := regexp.MustCompile(`^(.+?)\s*\(([^)]+)\)\s*$`)
	matches := re.FindStringSubmatch(content)

	if len(matches) == 3 {
		gameName := strings.TrimSpace(matches[1])
		coreName := strings.TrimSpace(matches[2])

		// Map core name to friendly system name
		systemName := sw.mapCoreToSystemName(coreName)

		gameInfo := &SAMGameInfo{
			GameName:   gameName,
			SystemName: systemName,
			IsActive:   true,
			LastUpdate: func() time.Time {
				if fileInfo != nil {
					return fileInfo.ModTime()
				}
				return time.Now()
			}(),
		}

		sw.logger.Debug("sam watcher: ✅ parsed successfully - Game: '%s', Core: '%s', System: '%s'",
			gameName, coreName, systemName)
		return gameInfo, nil
	}

	// If format doesn't match expected pattern, log warning but try to use content
	sw.logger.Warn("sam watcher: ⚠️ unexpected format in SAM_Game.txt: '%s'", content)

	gameInfo := &SAMGameInfo{
		GameName:   content,
		SystemName: "Unknown",
		IsActive:   true,
		LastUpdate: func() time.Time {
			if fileInfo != nil {
				return fileInfo.ModTime()
			}
			return time.Now()
		}(),
	}

	return gameInfo, nil
}

// mapCoreToSystemName maps MiSTer core names to friendly system names
func (sw *SAMWatcher) mapCoreToSystemName(coreName string) string {
	// Core name to friendly system name mapping based on names.txt
	coreMapping := map[string]string{
		// Common cores that SAM uses frequently
		"tgfx16":             "Turbo Duo",
		"tgfx16cd":           "TGFX-16/CD LLAPI",
		"TurboGrafx16":       "Turbo Duo",
		"TurboGrafx16_LLAPI": "TGFX-16/CD LLAPI",
		"arcade":             "Arcade",
		"ARCADE":             "Arcade",
		"Genesis":            "Genesis +",
		"MegaDrive":          "Genesis",
		"SNES":               "Super NES",
		"NES":                "NES",
		"GBA":                "Game Boy Adv. +",
		"Gameboy":            "Game Boy",
		"GameboyColor":       "Game Boy Color",
		"PSX":                "PlayStation",
		"N64":                "N64",
		"SMS":                "Master System",
		"Game Gear":          "Game Gear",
		"AtariLynx":          "Atari Lynx",
		"NeoGeo":             "Neo Geo MVS/AES",
		"C64":                "Commodore 64",
		"Minimig":            "Commodore Amiga",
		"ao486":              "PC (486SX)",
		"MegaCD":             "SEGA CD",
		"Saturn":             "Saturn",
		"S32X":               "Genesis 32X",

		// Full mapping from names.txt
		"240pSuite":           "240p Test Suite",
		"AcornAtom":           "Atom",
		"AcornElectron":       "Electron",
		"ADCTest":             "ADC Input Test",
		"AdventureVision":     "Adventure Vision",
		"AliceMC10":           "Tandy MC-10",
		"Alpha68k":            "SNK Alpha 68000",
		"Altair8800":          "Altair 8800",
		"Amstrad":             "Amstrad CPC",
		"Amstrad-PCW":         "Amstrad PCW",
		"Apogee":              "Apogee BK-01",
		"Apple-I":             "Apple I",
		"Apple-II":            "Apple IIe",
		"Aquarius":            "Mattel Aquarius",
		"Arcadia":             "Arcadia 2001",
		"Archie":              "Acorn Archimedes",
		"Arduboy":             "Arduboy",
		"Astrocade":           "Bally Astrocade",
		"Atari 2600":          "Atari 2600",
		"Atari5200":           "Atari 5200",
		"Atari7800":           "Atari 7800",
		"Atari7800_Sinden":    "Atari 7800 S",
		"Atari800":            "Atari 800XL/...",
		"AtariST":             "Atari ST/STE",
		"AY-3-8500":           "AY-3-8500",
		"BBCBridgeCompanion":  "Bridge Companion",
		"BBCMicro":            "BBC Micro/Master",
		"BK0011M":             "BK0011M",
		"C128":                "Commodore 128",
		"C16":                 "Commodore 16",
		"C2650":               "Arcadia 2001",
		"Casio_PV-1000":       "PV-1000",
		"Casio_PV-2000":       "PV-2000",
		"CDi":                 "CD-i",
		"ChannelF":            "Channel F",
		"Chess":               "Chess",
		"Chip8":               "CHIP-8",
		"CoCo2":               "TRS-80 CoCo 2",
		"CoCo3":               "TRS-80 CoCo 3",
		"ColecoAdam":          "Coleco Adam",
		"ColecoVision":        "ColecoVision",
		"CreatiVision":        "CreatiVision",
		"Donut":               "VGA Donut",
		"EDSAC":               "EDSAC",
		"eg2000":              "EG2000",
		"EpochGalaxyII":       "Galaxy II",
		"F68kMiSTer":          "X68000",
		"FlappyBird":          "Flappy Bird",
		"Galaksija":           "Galaksija",
		"Gamate":              "Gamate",
		"GameAndWatch":        "Game & Watch agg23",
		"Gameboy2P":           "Game Boy/Color 2P",
		"Gameboy2Pultrawide":  "Game Boy/Color 2PW",
		"Gameboy_LLAPI":       "GB/GBC LLAPI",
		"GameOfLife":          "Game of Life",
		"GBA2P":               "Game Boy Adv. + 2P",
		"GBA_accuracy":        "Game Boy Advance",
		"GBA_LLAPI":           "GBA + LLAPI",
		"Genesis3D":           "Genesis + 3D",
		"Genesis_LLAPI":       "Genesis + LLAPI",
		"Genesis_Sinden":      "Genesis + S",
		"GnW":                 "Game & Watch",
		"Hack":                "Nand2Tetris Hack",
		"Homelab":             "Homelab Series",
		"InputTest":           "Input Test",
		"InputTest_Sinden":    "Input Test S",
		"Intellivision":       "Intellivision",
		"Interact":            "Interact",
		"Intv":                "Intellivision",
		"Jaguar":              "Atari Jaguar",
		"jtsdram48":           "SDRAM Test 48 MHz",
		"jtsdram96":           "SDRAM Test 96 MHz",
		"Jupiter":             "Jupiter Ace",
		"KC87":                "Robotron KC 87",
		"Konix":               "Multisystem",
		"Laser310":            "Laser 350/500/700",
		"Lynx48":              "Lynx 48/96K",
		"MacPlus":             "Macintosh Plus",
		"mandelbrot":          "Mandelbrot Zoom",
		"Mega Duck":           "Mega Duck",
		"MegaCD_LLAPI":        "SEGA CD LLAPI",
		"MegaCD_Sinden":       "SEGA CD S",
		"MegaDrive_Sinden":    "Genesis S",
		"Memotech-MTX":        "Memotech MTX",
		"memtest":             "SDRAM Test",
		"mister_coco":         "TRS-80 CoCo 3",
		"MiSTerLaggy":         "MiSTer Laggy",
		"MSX":                 "MSX (1chipMSX)",
		"MSX1":                "MSX1",
		"MultiComp":           "MultiComp",
		"MyVision":            "My Vision",
		"NeoGeo_LLAPI":        "Neo Geo LLAPI",
		"NeoGeoPocket":        "Neo Geo Pocket",
		"NES3D":               "NES 3D",
		"NES_LLAPI":           "NES LLAPI",
		"NES_Sinden":          "NES S",
		"Odyssey2":            "Magnavox Odyssey2",
		"Ondra_SPO186":        "Ondra SPO 186",
		"ORAO":                "Orao",
		"Oric":                "Oric",
		"Pacman_LLAPI":        "Pac-Man LLAPI",
		"PC88":                "PC-8801",
		"PCXT":                "PC/XT",
		"PDP1":                "PDP-1",
		"PET2001":             "Commodore PET 2001",
		"PICO":                "Kids Computer Pico",
		"PMD85":               "PMD 85-2A",
		"Pocket Challenge V2": "Pocket Challenge 2",
		"PokemonMini":         "Pokemon mini",
		"PSX_Sinden":          "PlayStation S",
		"QL":                  "Sinclair QL",
		"RX78":                "RX-78 Gundam",
		"SAMCoupe":            "SAM Coupe",
		"SCV":                 "SCV",
		"SGB":                 "Super Game Boy",
		"SharpMZ":             "Sharp MZ",
		"SlugCross":           "Slug Cross",
		"SMS3D":               "Master System 3D",
		"SMS_LLAPI":           "SMS LLAPI",
		"SMS_Sinden":          "Master System S",
		"SNES_LLAPI":          "SNES LLAPI",
		"SNES_Sinden":         "Super NES S",
		"SordM5":              "M5",
		"Specialist":          "Specialist/MX",
		"SS":                  "SPARCstation",
		"Super_Vision_8000":   "Super Vision 8000",
		"SuperJacob":          "Super Jacob",
		"SuperVision":         "SuperVision",
		"Svi328":              "SV-328",
		"System1":             "System 1",
		"TatungEinstein":      "Einstein",
		"Ti994a":              "TI-99/4A",
		"TomyScramble":        "Tomy Scramble",
		"TomyTutor":           "Tutor",
		"TRS-80":              "TRS-80",
		"TSConf":              "TS-Config",
		"UK101":               "UK101",
		"VC4000":              "VC 4000",
		"Vector-06C":          "Vector-06C",
		"Vectrex":             "Vectrex",
		"VIC20":               "Commodore VIC-20",
		"VT52":                "VT52 Terminal",
		"WonderSwan":          "WonderSwan",
		"WonderSwan Color":    "WonderSwan Color",
		"X68000":              "X68000",
		"zet98":               "PC-98",
		"ZX-Spectrum":         "ZX Spectrum",
		"zx48":                "ZX48",
		"ZX81":                "TS-1500",
		"ZXNext":              "ZX Spectrum Next",
	}

	// Try exact match first
	if systemName, exists := coreMapping[coreName]; exists {
		return systemName
	}

	// Try case-insensitive match
	lowerCoreName := strings.ToLower(coreName)
	for core, system := range coreMapping {
		if strings.ToLower(core) == lowerCoreName {
			return system
		}
	}

	// If no mapping found, return the core name cleaned up
	sw.logger.Debug("sam watcher: no mapping found for core '%s', using as-is", coreName)
	return coreName
}

// checkSAMGameChange specifically handles potential game changes (triggered by file events)
func (sw *SAMWatcher) checkSAMGameChange() {
	// First verify SAM is actually active
	if !sw.IsSAMActive() {
		sw.logger.Info("sam watcher: ⚠️ File changed but SAM not active - ignoring")
		sw.handleSAMStop()
		return
	}

	gameInfo, err := sw.parseSAMGameFile()
	if err != nil {
		sw.logger.Error("sam watcher: failed to parse SAM_Game.txt after change: %s", err)
		return
	}

	if gameInfo == nil {
		sw.logger.Debug("sam watcher: SAM_Game.txt is empty after change")
		return
	}

	// Always check for game changes when file event occurs
	isNewGame := sw.currentGame == nil ||
		sw.currentGame.GameName != gameInfo.GameName ||
		sw.currentGame.SystemName != gameInfo.SystemName

	if isNewGame {
		sw.logger.Info("sam watcher: 🎯 NEW GAME DETECTED: '%s' (%s) [previous: %s]",
			gameInfo.GameName, gameInfo.SystemName,
			func() string {
				if sw.currentGame != nil {
					return sw.currentGame.GameName
				}
				return "none"
			}())

		sw.currentGame = gameInfo
		sw.updateTrackerWithSAMGame(gameInfo)

		// Notify about game change
		sw.notifyGameChange(gameInfo)
	} else {
		// Same game, but update timestamp to keep it fresh
		if sw.currentGame != nil {
			sw.currentGame.LastUpdate = gameInfo.LastUpdate
			sw.logger.Debug("sam watcher: same game '%s', timestamp updated", gameInfo.GameName)
		}
	}
}

// periodicSAMCheck performs periodic verification (less verbose, just to catch missed events)
func (sw *SAMWatcher) periodicSAMCheck() {
	if !sw.IsSAMActive() {
		if sw.currentGame != nil {
			sw.logger.Info("sam watcher: 🔄 Periodic check - SAM no longer active")
			sw.handleSAMStop()
		}
		return
	}

	gameInfo, err := sw.parseSAMGameFile()
	if err != nil || gameInfo == nil {
		return
	}

	// Check if we missed a game change
	if sw.currentGame == nil ||
		sw.currentGame.GameName != gameInfo.GameName ||
		sw.currentGame.SystemName != gameInfo.SystemName {

		sw.logger.Info("sam watcher: 🔄 Periodic check found missed game change: '%s'", gameInfo.GameName)
		sw.currentGame = gameInfo
		sw.updateTrackerWithSAMGame(gameInfo)
		sw.notifyGameChange(gameInfo)
	}
}

// updateTrackerWithSAMGame updates the tracker with SAM game information
func (sw *SAMWatcher) updateTrackerWithSAMGame(gameInfo *SAMGameInfo) {
	sw.tracker.mu.Lock()
	defer sw.tracker.mu.Unlock()

	// Update tracker with SAM game info - SAM provides all the correct information
	sw.tracker.ActiveGameName = gameInfo.GameName
	sw.tracker.ActiveSystemName = gameInfo.SystemName

	// Special handling for arcade games - they don't follow the same pattern
	isArcade := sw.isArcadeSystem(gameInfo.SystemName)

	if isArcade {
		// For arcade games, ActiveGame and ActiveCore should be the game name
		sw.tracker.ActiveGame = gameInfo.GameName
		sw.tracker.ActiveCore = gameInfo.GameName
	} else {
		// For console games, maintain the system/game format
		sw.tracker.ActiveGame = fmt.Sprintf("%s/%s",
			gameInfo.SystemName, gameInfo.GameName)

		// Try to get actual core name from tracker, fallback to system name
		if sw.tracker.ActiveCore == "" {
			sw.tracker.ActiveCore = gameInfo.SystemName
		}
	}

	// Set a marker that this data comes from SAM
	sw.tracker.ActiveGamePath = "SAM:" + gameInfo.GameName

	sw.logger.Info("sam watcher: tracker updated - Game: %s, System: %s, IsArcade: %v",
		gameInfo.GameName, gameInfo.SystemName, isArcade)
}

// isArcadeSystem determines if a system name represents an arcade system
func (sw *SAMWatcher) isArcadeSystem(systemName string) bool {
	arcadeSystems := []string{
		"Arcade",
		"Neo Geo MVS/AES",
		"Neo Geo LLAPI",
		"SNK Alpha 68000",
		"System 1",
		"Pac-Man LLAPI",
	}

	systemLower := strings.ToLower(systemName)
	for _, arcade := range arcadeSystems {
		if strings.ToLower(arcade) == systemLower {
			return true
		}
	}

	// Also check if it contains "arcade" anywhere
	return strings.Contains(systemLower, "arcade")
}

// handleSAMStop handles when SAM stops or the game file is removed
func (sw *SAMWatcher) handleSAMStop() {
	if sw.currentGame != nil {
		sw.logger.Info("sam watcher: SAM stopped, clearing SAM game info")
		sw.currentGame = nil

		// Clear SAM-specific tracker info, let normal detection take over
		sw.tracker.mu.Lock()
		if strings.HasPrefix(sw.tracker.ActiveGamePath, "SAM:") {
			sw.tracker.ActiveGamePath = ""
			sw.tracker.ActiveGame = ""
			sw.tracker.ActiveGameName = ""
		}
		sw.tracker.mu.Unlock()
	}
}

// notifyGameChange sends notifications about game changes
func (sw *SAMWatcher) notifyGameChange(gameInfo *SAMGameInfo) {
	// This could be extended to send websocket notifications or other events
	sw.logger.Info("sam watcher: 📢 GAME CHANGE NOTIFICATION: %s (%s)",
		gameInfo.GameName, gameInfo.SystemName)

	// Future: Could add websocket broadcast here
	// websocket.Broadcast(sw.logger, "samGameChange:"+gameInfo.GameName)
}

// GetCurrentSAMGame returns the current SAM game info with additional metadata
func (sw *SAMWatcher) GetCurrentSAMGame() *SAMGameInfo {
	if sw.currentGame != nil {
		// Create a copy to avoid modification of internal state
		return &SAMGameInfo{
			GameName:   sw.currentGame.GameName,
			SystemName: sw.currentGame.SystemName,
			IsActive:   sw.IsSAMActive(), // Real-time active check
			LastUpdate: sw.currentGame.LastUpdate,
		}
	}
	return nil
}

// GetGameChangeStatistics could be added for debugging/analytics
func (sw *SAMWatcher) GetGameChangeStatistics() map[string]interface{} {
	stats := map[string]interface{}{
		"sam_active":      sw.IsSAMActive(),
		"current_game":    sw.GetCurrentSAMGame(),
		"watcher_running": sw.watcher != nil,
	}

	// Add file stats
	if info, err := os.Stat(SAMGameFile); err == nil {
		stats["file_info"] = map[string]interface{}{
			"exists":        true,
			"last_modified": info.ModTime(),
			"seconds_since": time.Since(info.ModTime()).Seconds(),
			"size_bytes":    info.Size(),
		}
	} else {
		stats["file_info"] = map[string]interface{}{
			"exists": false,
			"error":  err.Error(),
		}
	}

	return stats
}
