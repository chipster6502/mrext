package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jung-kurt/gofpdf"
	"github.com/wizzomafizzo/mrext/pkg/config"
	"github.com/wizzomafizzo/mrext/pkg/games"
	"github.com/wizzomafizzo/mrext/pkg/service"
	"github.com/wizzomafizzo/mrext/pkg/tracker"
)

// GameCache stores preprocessed game data with aggressive timeouts
type GameCache struct {
	Games       []InstalledGame `json:"games"`
	LastUpdated time.Time       `json:"last_updated"`
	mutex       sync.RWMutex
}

var (
	gameCache = &GameCache{}
	cacheTTL  = 15 * time.Minute // Cache valid for 15 minutes
)

// HandleChat processes interactive chat requests with Claude
func HandleChat(logger *service.Logger, cfg *config.UserConfig, trk *tracker.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Verify Claude is enabled
		if !cfg.Claude.Enabled {
			http.Error(w, "Claude is not enabled", http.StatusServiceUnavailable)
			return
		}

		// Validate API key
		if cfg.Claude.APIKey == "" {
			http.Error(w, "Claude API key not configured", http.StatusServiceUnavailable)
			return
		}

		// Parse request
		var request ChatRequest
		err := json.NewDecoder(r.Body).Decode(&request)
		if err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			logger.Error("claude chat: failed to decode request: %s", err)
			return
		}

		// Validate message
		if request.Message == "" {
			http.Error(w, "Message cannot be empty", http.StatusBadRequest)
			return
		}

		logger.Info("claude chat: processing message from session %s", request.SessionID)

		// Create Claude client
		client := NewClient(&cfg.Claude, logger)

		// Get game context if requested
		var gameContext *GameContext
		if request.IncludeContext && trk != nil {
			gameContext = client.buildGameContext(trk)
		}

		// Create context with timeout
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(cfg.Claude.TimeoutSeconds)*time.Second)
		defer cancel()

		// Send message to Claude
		response, err := client.SendMessage(ctx, request.Message, gameContext, request.SessionID)
		if err != nil {
			logger.Error("claude chat: %s", err)
			http.Error(w, "Failed to process chat request", http.StatusInternalServerError)
			return
		}

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Encode and send response
		err = json.NewEncoder(w).Encode(response)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			logger.Error("claude chat: failed to encode response: %s", err)
			return
		}

		logger.Info("claude chat: response sent successfully")
	}
}

// HandleSuggestions generates automatic game suggestions
func HandleSuggestions(logger *service.Logger, cfg *config.UserConfig, trk *tracker.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Verify Claude is enabled
		if !cfg.Claude.Enabled {
			http.Error(w, "Claude is not enabled", http.StatusServiceUnavailable)
			return
		}

		// Check if auto suggestions are enabled
		if !cfg.Claude.AutoSuggestions {
			response := &SuggestionsResponse{
				Suggestions: []string{},
				Timestamp:   time.Now(),
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
			return
		}

		// Validate API key
		if cfg.Claude.APIKey == "" {
			http.Error(w, "Claude API key not configured", http.StatusServiceUnavailable)
			return
		}

		logger.Info("claude suggestions: generating for current game")

		// Create Claude client
		client := NewClient(&cfg.Claude, logger)

		// Create context with timeout
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(cfg.Claude.TimeoutSeconds)*time.Second)
		defer cancel()

		// Generate suggestions
		response, err := client.GenerateSuggestions(ctx, trk)
		if err != nil {
			logger.Error("claude suggestions: %s", err)
			http.Error(w, "Failed to generate suggestions", http.StatusInternalServerError)
			return
		}

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Encode and send response
		err = json.NewEncoder(w).Encode(response)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			logger.Error("claude suggestions: failed to encode response: %s", err)
			return
		}

		logger.Info("claude suggestions: %d suggestions generated", len(response.Suggestions))
	}
}

// HandlePlaylist generates themed game playlists (FAST & OPTIMIZED)
func HandlePlaylist(logger *service.Logger, cfg *config.UserConfig, trk *tracker.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		startTime := time.Now()

		// Verify Claude is enabled
		if !cfg.Claude.Enabled {
			http.Error(w, "Claude is not enabled", http.StatusServiceUnavailable)
			return
		}

		// Validate API key
		if cfg.Claude.APIKey == "" {
			http.Error(w, "Claude API key not configured", http.StatusServiceUnavailable)
			return
		}

		// Parse request
		var request PlaylistRequest
		err := json.NewDecoder(r.Body).Decode(&request)
		if err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			logger.Error("claude playlist: failed to decode request: %s", err)
			return
		}

		// Validate request
		if request.Theme == "" {
			http.Error(w, "Theme cannot be empty", http.StatusBadRequest)
			return
		}

		if request.GameCount <= 0 || request.GameCount > 15 {
			request.GameCount = 5 // Reduced max
		}

		logger.Info("claude playlist: generating %d games for theme '%s' from systems: %v",
			request.GameCount, request.Theme, request.Systems)

		// ✅ FAST: Get games with aggressive timeout (max 8 seconds for scanning)
		scanTimeout := 8 * time.Second
		installedGames, err := getInstalledGamesFast(cfg, logger, request.Systems, scanTimeout)
		if err != nil {
			logger.Error("claude playlist: failed to get installed games: %s", err)
			http.Error(w, "Failed to scan game collection", http.StatusInternalServerError)
			return
		}

		if len(installedGames) == 0 {
			http.Error(w, "No games found in selected systems", http.StatusNotFound)
			return
		}

		// ✅ SMART FILTERING: Reduce dataset for Claude (max 50 games)
		maxGamesForClaude := 50
		filteredGames := smartFilterGames(installedGames, request.Theme, maxGamesForClaude)

		logger.Info("claude playlist: filtered to %d games from %d total (scan took %v)",
			len(filteredGames), len(installedGames), time.Since(startTime))

		request.InstalledGames = filteredGames

		// Create Claude client
		client := NewClient(&cfg.Claude, logger)

		// Create context with timeout (remaining time)
		remainingTime := time.Duration(cfg.Claude.TimeoutSeconds)*time.Second - time.Since(startTime)
		if remainingTime < 5*time.Second {
			remainingTime = 5 * time.Second // Minimum 5 seconds for Claude
		}

		ctx, cancel := context.WithTimeout(r.Context(), remainingTime)
		defer cancel()

		// Check if this is an active game-based playlist request
		var response *PlaylistResponse
		if isActiveGameThemeKeyword(request.Theme) {
			// Use active game-based playlist generation
			response, err = client.GeneratePlaylistFromActiveGame(ctx, &request, trk)
		} else {
			// Use standard playlist generation
			response, err = client.GeneratePlaylist(ctx, &request)
		}
		if err != nil {
			logger.Error("claude playlist: %s", err)
			http.Error(w, "Failed to generate playlist", http.StatusInternalServerError)
			return
		}

		// ✅ Add metadata for export functionality
		if response.Error == "" {
			for i := range response.Games {
				response.Games[i].GeneratedAt = time.Now()
				response.Games[i].Theme = request.Theme
			}
		}

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Encode and send response
		err = json.NewEncoder(w).Encode(response)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			logger.Error("claude playlist: failed to encode response: %s", err)
			return
		}

		totalTime := time.Since(startTime)
		logger.Info("claude playlist: completed %d games for theme '%s' in %v",
			len(response.Games), request.Theme, totalTime)
	}
}

// ✅ FAST: Get installed games with aggressive timeout (FIXED - Anti Alphabetical Bias)
func getInstalledGamesFast(cfg *config.UserConfig, logger *service.Logger, systems []string, timeout time.Duration) ([]InstalledGame, error) {
	start := time.Now()
	deadline := start.Add(timeout)

	// Use cached games if available and recent
	gameCache.mutex.RLock()
	if len(gameCache.Games) > 0 && time.Since(gameCache.LastUpdated) < 30*time.Second {
		games := make([]InstalledGame, len(gameCache.Games))
		copy(games, gameCache.Games)
		gameCache.mutex.RUnlock()
		logger.Info("claude playlist: using cached games (%d games)", len(games))

		// ✅ CRITICAL: Even cached games need randomization to prevent session bias
		return randomizeGames(games), nil
	}
	gameCache.mutex.RUnlock()

	// Scan for new games
	gameCache.mutex.Lock()
	defer gameCache.mutex.Unlock()

	targetSystems := make([]games.System, 0)
	if len(systems) == 0 {
		targetSystems = games.AllSystems()
	} else {
		for _, sysName := range systems {
			if sys, err := games.LookupSystem(sysName); err == nil {
				targetSystems = append(targetSystems, *sys)
			}
		}
	}

	logger.Info("claude playlist: scanning %d systems with %v timeout", len(targetSystems), timeout)

	systemPaths := games.GetSystemPaths(cfg, targetSystems)
	var installedGames []InstalledGame

	// Group paths by system
	systemPathsMap := make(map[string][]string)
	for _, p := range systemPaths {
		systemPathsMap[p.System.Id] = append(systemPathsMap[p.System.Id], p.Path)
	}

	// Scan with strict timeout per system
	maxGamesPerSystem := 100 // Aggressive limit
	systemTimeout := timeout / time.Duration(len(systemPathsMap))
	if systemTimeout < 1*time.Second {
		systemTimeout = 1 * time.Second
	}

	for systemId, paths := range systemPathsMap {
		if time.Now().After(deadline) {
			logger.Warn("claude playlist: global timeout reached, stopping scan")
			break
		}

		systemStart := time.Now()
		systemDeadline := systemStart.Add(systemTimeout)

		system, err := games.LookupSystem(systemId)
		if err != nil {
			continue
		}

		systemGameCount := 0

		for _, path := range paths {
			if time.Now().After(systemDeadline) {
				logger.Debug("claude playlist: system timeout for %s", systemId)
				break
			}

			files, err := games.GetFiles(systemId, path)
			if err != nil {
				logger.Debug("failed to scan %s: %s", path, err)
				continue
			}

			// ✅ CRITICAL FIX: Randomize files immediately after GetFiles() to break filesystem alphabetical order
			files = randomizeFileList(files)

			// Process files with limit
			for _, file := range files {
				if systemGameCount >= maxGamesPerSystem || time.Now().After(systemDeadline) {
					break
				}

				gameName := extractGameName(file)
				installedGames = append(installedGames, InstalledGame{
					Name:   gameName,
					Path:   file,
					System: system.Name,
				})
				systemGameCount++
			}

			if systemGameCount >= maxGamesPerSystem {
				break
			}
		}

		systemTime := time.Since(systemStart)
		logger.Info("claude playlist: scanned %s: %d games in %v", systemId, systemGameCount, systemTime)
	}

	// ✅ SECOND RANDOMIZATION: Randomize the entire collection before caching
	installedGames = randomizeGames(installedGames)

	// Update cache
	gameCache.Games = installedGames
	gameCache.LastUpdated = time.Now()

	elapsed := time.Since(start)
	logger.Info("claude playlist: fast scan completed: %d games from %d systems in %v",
		len(installedGames), len(systemPathsMap), elapsed)

	return installedGames, nil
}

// ✅ NEW: Randomize file list to break filesystem alphabetical ordering
func randomizeFileList(files []string) []string {
	if len(files) <= 1 {
		return files
	}

	randomized := make([]string, len(files))
	copy(randomized, files)

	// Use time-based seed to ensure different randomization each call
	baseTime := time.Now().UnixNano()
	for i := len(randomized) - 1; i > 0; i-- {
		// Create pseudo-random but deterministic-ish index
		seed := baseTime + int64(i*1000) + int64(len(randomized)*100)
		j := int(seed) % (i + 1)
		if j < 0 {
			j = -j
		}
		randomized[i], randomized[j] = randomized[j], randomized[i]
	}

	return randomized
}

// ✅ NEW: Randomize InstalledGame slice to break any remaining bias
func randomizeGames(games []InstalledGame) []InstalledGame {
	if len(games) <= 1 {
		return games
	}

	randomized := make([]InstalledGame, len(games))
	copy(randomized, games)

	// Use time-based seed with different offset than file randomization
	baseTime := time.Now().UnixNano() + 9999 // Different seed than file randomization
	for i := len(randomized) - 1; i > 0; i-- {
		seed := baseTime + int64(i*2000) + int64(len(randomized)*200)
		j := int(seed) % (i + 1)
		if j < 0 {
			j = -j
		}
		randomized[i], randomized[j] = randomized[j], randomized[i]
	}

	return randomized
}

// ✅ FAST: Build game cache with strict timeout
func buildGameCacheFast(cfg *config.UserConfig, logger *service.Logger, systemIds []string, timeout time.Duration) ([]InstalledGame, error) {
	gameCache.mutex.Lock()
	defer gameCache.mutex.Unlock()

	start := time.Now()
	deadline := start.Add(timeout)

	// Convert system IDs to system objects
	var targetSystems []games.System
	for _, systemId := range systemIds {
		if system, err := games.LookupSystem(systemId); err == nil {
			targetSystems = append(targetSystems, *system)
		}
	}

	if len(targetSystems) == 0 {
		return nil, fmt.Errorf("no valid systems specified")
	}

	logger.Info("claude playlist: fast scanning %d systems with %v timeout", len(targetSystems), timeout)

	systemPaths := games.GetSystemPaths(cfg, targetSystems)
	var installedGames []InstalledGame

	// Group paths by system
	systemPathsMap := make(map[string][]string)
	for _, p := range systemPaths {
		systemPathsMap[p.System.Id] = append(systemPathsMap[p.System.Id], p.Path)
	}

	// Scan with strict timeout per system
	maxGamesPerSystem := 100 // Aggressive limit
	systemTimeout := timeout / time.Duration(len(systemPathsMap))
	if systemTimeout < 1*time.Second {
		systemTimeout = 1 * time.Second
	}

	for systemId, paths := range systemPathsMap {
		if time.Now().After(deadline) {
			logger.Warn("claude playlist: global timeout reached, stopping scan")
			break
		}

		systemStart := time.Now()
		systemDeadline := systemStart.Add(systemTimeout)

		system, err := games.LookupSystem(systemId)
		if err != nil {
			continue
		}

		systemGameCount := 0

		for _, path := range paths {
			if time.Now().After(systemDeadline) {
				logger.Debug("claude playlist: system timeout for %s", systemId)
				break
			}

			files, err := games.GetFiles(systemId, path)
			if err != nil {
				logger.Debug("failed to scan %s: %s", path, err)
				continue
			}

			// Process files with limit
			for _, file := range files {
				if systemGameCount >= maxGamesPerSystem || time.Now().After(systemDeadline) {
					break
				}

				gameName := extractGameName(file)
				installedGames = append(installedGames, InstalledGame{
					Name:   gameName,
					Path:   file,
					System: system.Name,
				})
				systemGameCount++
			}

			if systemGameCount >= maxGamesPerSystem {
				break
			}
		}

		systemTime := time.Since(systemStart)
		logger.Info("claude playlist: scanned %s: %d games in %v", systemId, systemGameCount, systemTime)
	}

	// Update cache
	gameCache.Games = installedGames
	gameCache.LastUpdated = time.Now()

	elapsed := time.Since(start)
	logger.Info("claude playlist: fast scan completed: %d games from %d systems in %v",
		len(installedGames), len(systemPathsMap), elapsed)

	return installedGames, nil
}

// ✅ SMART: Pre-filter games by theme relevance (FIXED - no alphabetical bias)
func smartFilterGames(games []InstalledGame, theme string, maxGames int) []InstalledGame {
	if len(games) <= maxGames {
		// ✅ Even for small collections, randomize to prevent bias
		randomized := make([]InstalledGame, len(games))
		copy(randomized, games)

		// Simple time-based shuffle for small collections
		for i := len(randomized) - 1; i > 0; i-- {
			j := int(time.Now().UnixNano()) % (i + 1)
			randomized[i], randomized[j] = randomized[j], randomized[i]
		}
		return randomized
	}

	themeKeywords := extractThemeKeywords(theme)

	// ✅ Group games by score to handle ties properly
	scoreGroups := make(map[int][]InstalledGame)
	maxScore := 0

	for _, game := range games {
		score := calculateGameThemeScore(game, themeKeywords)
		if score > maxScore {
			maxScore = score
		}
		scoreGroups[score] = append(scoreGroups[score], game)
	}

	// ✅ Randomize within each score group to eliminate alphabetical bias
	for score, gameList := range scoreGroups {
		shuffled := make([]InstalledGame, len(gameList))
		copy(shuffled, gameList)

		// Time-based shuffle for each score group
		baseTime := time.Now().UnixNano()
		for i := len(shuffled) - 1; i > 0; i-- {
			// Use score + index + time to ensure different randomization per group
			seed := baseTime + int64(score*1000) + int64(i*100)
			j := int(seed) % (i + 1)
			if j < 0 {
				j = -j
			}
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		}

		scoreGroups[score] = shuffled
	}

	// ✅ Collect results by score (highest first), but randomized within each score
	result := make([]InstalledGame, 0, maxGames)

	// Iterate through scores from highest to lowest
	for score := maxScore; score >= 0 && len(result) < maxGames; score-- {
		if gameList, exists := scoreGroups[score]; exists {
			for _, game := range gameList {
				if len(result) >= maxGames {
					break
				}
				result = append(result, game)
			}
		}
	}

	return result
}

// ✅ EXTRACT: Theme keywords for smart filtering
func extractThemeKeywords(theme string) []string {
	theme = strings.ToLower(theme)
	keywords := []string{}

	// Gaming genre/style keywords
	gameKeywords := map[string][]string{
		"action":     {"action", "shooter", "shoot", "gun", "fight", "combat", "battle", "war"},
		"puzzle":     {"puzzle", "tetris", "block", "match", "brain", "logic"},
		"platformer": {"platform", "jump", "mario", "sonic", "run", "side"},
		"rpg":        {"rpg", "role", "final", "fantasy", "dragon", "quest", "adventure"},
		"racing":     {"racing", "drive", "car", "speed", "race", "formula", "grand"},
		"arcade":     {"arcade", "classic", "retro", "coin", "cabinet"},
		"sports":     {"sport", "football", "baseball", "basketball", "soccer", "tennis"},
		"strategy":   {"strategy", "tactical", "war", "civilization", "empire"},
	}

	for _, genreWords := range gameKeywords {
		for _, word := range genreWords {
			if strings.Contains(theme, word) {
				keywords = append(keywords, word)
			}
		}
	}

	// Add direct theme words
	words := strings.Fields(theme)
	for _, word := range words {
		if len(word) > 2 {
			keywords = append(keywords, strings.ToLower(word))
		}
	}

	return keywords
}

// ✅ SCORE: Calculate game relevance to theme
func calculateGameThemeScore(game InstalledGame, keywords []string) int {
	score := 0
	gameName := strings.ToLower(game.Name)

	// Direct keyword matches
	for _, keyword := range keywords {
		if strings.Contains(gameName, keyword) {
			score += 15 // High bonus for direct matches
		}
	}

	// Popular game bonuses
	popularGames := []string{"mario", "sonic", "zelda", "street", "final", "mega", "contra"}
	for _, popular := range popularGames {
		if strings.Contains(gameName, popular) {
			score += 10
		}
	}

	// System popularity bonuses
	systemBonus := map[string]int{
		"NES":              5,
		"SNES":             5,
		"Genesis":          5,
		"Game Boy Advance": 4,
		"Arcade":           8, // Arcade games often have good variety
	}

	if bonus, exists := systemBonus[game.System]; exists {
		score += bonus
	}

	// Length penalty for very long names (often compilations)
	if len(game.Name) > 30 {
		score -= 3
	}

	return score
}

// ✅ FILTER: Games by specific systems
func filterGamesBySystem(games []InstalledGame, systems []string) []InstalledGame {
	if len(systems) == 0 {
		return games
	}

	systemSet := make(map[string]bool)
	for _, system := range systems {
		systemSet[strings.ToLower(system)] = true
		// Add alternate names
		if system == "Genesis" {
			systemSet["sega genesis"] = true
			systemSet["mega drive"] = true
		}
		if system == "NES" {
			systemSet["nintendo entertainment system"] = true
		}
	}

	var filtered []InstalledGame
	for _, game := range games {
		if systemSet[strings.ToLower(game.System)] {
			filtered = append(filtered, game)
		}
	}

	return filtered
}

// ✅ NEW: Extract clean game name from file path
func extractGameName(filePath string) string {
	// Get just the filename
	fileName := filepath.Base(filePath)

	// Remove file extension
	name := strings.TrimSuffix(fileName, filepath.Ext(fileName))

	// Remove common numeric prefixes like "003 "
	re := regexp.MustCompile(`^\d+\s+`)
	name = re.ReplaceAllString(name, "")

	// Remove common suffixes like (USA), (Europe), [!], etc.
	re = regexp.MustCompile(`\s*[\(\[][^\)\]]*[\)\]]\s*`)
	name = re.ReplaceAllString(name, "")

	// Replace underscores with spaces and clean up
	name = strings.ReplaceAll(name, "_", " ")
	name = strings.TrimSpace(name)

	return name
}

// HandleStatus provides Claude configuration status
func HandleStatus(logger *service.Logger, cfg *config.UserConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := map[string]interface{}{
			"enabled":          cfg.Claude.Enabled,
			"api_key_set":      cfg.Claude.APIKey != "",
			"model":            cfg.Claude.Model,
			"auto_suggestions": cfg.Claude.AutoSuggestions,
			"max_requests":     cfg.Claude.MaxRequestsPerHour,
			"chat_history":     cfg.Claude.ChatHistory,
			"timeout":          cfg.Claude.TimeoutSeconds,
		}

		w.Header().Set("Content-Type", "application/json")
		err := json.NewEncoder(w).Encode(status)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			logger.Error("claude status: failed to encode response: %s", err)
			return
		}
	}
}

// HandleUpdateConfig allows runtime configuration updates
func HandleUpdateConfig(logger *service.Logger, cfg *config.UserConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Parse configuration updates
		var updates map[string]interface{}
		err := json.NewDecoder(r.Body).Decode(&updates)
		if err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			logger.Error("claude config update: failed to decode request: %s", err)
			return
		}

		// Apply safe configuration updates
		updated := false

		if enabled, ok := updates["enabled"].(bool); ok {
			cfg.Claude.Enabled = enabled
			updated = true
			logger.Info("claude config: enabled set to %t", enabled)
		}

		if autoSugg, ok := updates["auto_suggestions"].(bool); ok {
			cfg.Claude.AutoSuggestions = autoSugg
			updated = true
			logger.Info("claude config: auto_suggestions set to %t", autoSugg)
		}

		if maxReq, ok := updates["max_requests_per_hour"].(float64); ok && maxReq > 0 {
			cfg.Claude.MaxRequestsPerHour = int(maxReq)
			updated = true
			logger.Info("claude config: max_requests_per_hour set to %d", int(maxReq))
		}

		if chatHist, ok := updates["chat_history"].(float64); ok && chatHist >= 0 {
			cfg.Claude.ChatHistory = int(chatHist)
			updated = true
			logger.Info("claude config: chat_history set to %d", int(chatHist))
		}

		if timeout, ok := updates["timeout_seconds"].(float64); ok && timeout > 0 {
			cfg.Claude.TimeoutSeconds = int(timeout)
			updated = true
			logger.Info("claude config: timeout_seconds set to %d", int(timeout))
		}

		// Validate updated configuration (if ValidateClaudeConfig exists)
		if updated {
			// Only call validation if the method exists
			// err = cfg.Claude.ValidateClaudeConfig()
			// if err != nil {
			//     http.Error(w, "Invalid configuration: "+err.Error(), http.StatusBadRequest)
			//     logger.Error("claude config update: validation failed: %s", err)
			//     return
			// }
		}

		// Return success response
		response := map[string]interface{}{
			"success": updated,
			"message": func() string {
				if updated {
					return "Configuration updated successfully"
				}
				return "No valid updates provided"
			}(),
		}

		w.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(w).Encode(response)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			logger.Error("claude config update: failed to encode response: %s", err)
			return
		}

		if updated {
			logger.Info("claude config: configuration updated successfully")
		}
	}
}

func HandleExportPlaylist(logger *service.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		format := r.URL.Query().Get("format")
		if format == "" {
			format = "txt"
		}

		var request struct {
			Games []GameRecommendation `json:"games"`
			Theme string               `json:"theme"`
		}

		err := json.NewDecoder(r.Body).Decode(&request)
		if err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if len(request.Games) == 0 {
			http.Error(w, "No games to export", http.StatusBadRequest)
			return
		}

		var content []byte
		var contentType string
		var filename string

		switch format {
		case "txt":
			textContent := formatPlaylistTXT(request.Games, request.Theme)
			content = []byte(textContent)
			contentType = "text/plain"
			filename = fmt.Sprintf("playlist_%s.txt", sanitizeFilename(request.Theme))
		case "pdf":
			pdfContent, err := formatPlaylistPDF(request.Games, request.Theme)
			if err != nil {
				logger.Error("claude playlist: failed to generate PDF: %s", err)
				http.Error(w, "Failed to generate PDF", http.StatusInternalServerError)
				return
			}
			content = pdfContent
			contentType = "application/pdf"
			filename = fmt.Sprintf("playlist_%s.pdf", sanitizeFilename(request.Theme))
		case "sync":
			syncContent := formatPlaylistSync(request.Games, request.Theme)
			content = []byte(syncContent)
			contentType = "text/plain"
			filename = fmt.Sprintf("playlist_%s.sync", sanitizeFilename(request.Theme))
		default:
			http.Error(w, "Unsupported format", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
		w.WriteHeader(http.StatusOK)
		w.Write(content)

		logger.Info("claude playlist: exported %d games as %s format", len(request.Games), format)
	}
}

// ✅ FORMAT: TXT format for playlist
func formatPlaylistTXT(games []GameRecommendation, theme string) string {
	var content strings.Builder

	content.WriteString(fmt.Sprintf("# Claude AI Playlist: %s\n", theme))
	content.WriteString(fmt.Sprintf("# Generated: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	content.WriteString(fmt.Sprintf("# Games: %d\n\n", len(games)))

	for i, game := range games {
		content.WriteString(fmt.Sprintf("%d. %s (%s)\n", i+1, game.Name, game.System))
		if game.Description != "" {
			content.WriteString(fmt.Sprintf("   Description: %s\n", game.Description))
		}
		if game.Reason != "" {
			content.WriteString(fmt.Sprintf("   Why: %s\n", game.Reason))
		}
		if game.Path != "" {
			content.WriteString(fmt.Sprintf("   Path: %s\n", game.Path))
		}
		content.WriteString("\n")
	}

	return content.String()
}

// ✅ UTILITY: Sanitize filename
func sanitizeFilename(name string) string {
	// Remove special characters
	reg := regexp.MustCompile(`[^a-zA-Z0-9\-_\s]`)
	name = reg.ReplaceAllString(name, "")

	// Replace spaces with underscores
	name = strings.ReplaceAll(name, " ", "_")

	// Limit length
	if len(name) > 30 {
		name = name[:30]
	}

	return strings.ToLower(name)
}

// ✅ UTILITY: Clear cache endpoint for debugging
func HandleClearCache(logger *service.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		gameCache.mutex.Lock()
		gameCache.Games = nil
		gameCache.LastUpdated = time.Time{}
		gameCache.mutex.Unlock()

		logger.Info("claude playlist: game cache cleared")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("Cache cleared"))
	}
}

// Helper function to parse integer query parameters
func parseIntParam(r *http.Request, param string, defaultValue int) int {
	value := r.URL.Query().Get(param)
	if value == "" {
		return defaultValue
	}

	parsed, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}

	return parsed
}

// Helper function to check if theme indicates active game-based playlist
func isActiveGameThemeKeyword(theme string) bool {
	theme = strings.ToLower(strings.TrimSpace(theme))
	keywords := []string{
		"active game",
		"current game",
		"similar to active",
		"similar to current",
		"based on active",
		"based on current",
		"like current game",
		"like active game",
		"playlist based in active game", // User's specific example
		"playlist based on active game",
	}

	for _, keyword := range keywords {
		if strings.Contains(theme, keyword) {
			return true
		}
	}
	return false
}

// HandleActiveGameSuggestion returns a dynamic suggestion based on current active game
func HandleActiveGameSuggestion(logger *service.Logger, cfg *config.UserConfig, trk *tracker.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Verify Claude is enabled
		if !cfg.Claude.Enabled {
			http.Error(w, "Claude is not enabled", http.StatusServiceUnavailable)
			return
		}

		// Create Claude client
		client := NewClient(&cfg.Claude, logger)

		// Get dynamic suggestion based on active game
		suggestion := client.GetActiveGameSuggestion(trk)

		response := map[string]interface{}{
			"suggestion": suggestion,
			"timestamp":  time.Now(),
		}

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Encode and send response
		err := json.NewEncoder(w).Encode(response)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			logger.Error("claude active game suggestion: failed to encode response: %s", err)
			return
		}

		logger.Info("claude active game suggestion: returned '%s'", suggestion)
	}
}

// HandleDebugActiveGame provides debugging info for active game detection
func HandleDebugActiveGame(logger *service.Logger, cfg *config.UserConfig, trk *tracker.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Create Claude client for debugging
		client := NewClient(&cfg.Claude, logger)

		// Build game context with full debugging
		gameContext := client.buildGameContext(trk)

		// Prepare debug response
		debugInfo := map[string]interface{}{
			"tracker_data": map[string]interface{}{
				"active_core":        trk.ActiveCore,
				"active_game":        trk.ActiveGame,
				"active_game_name":   trk.ActiveGameName,
				"active_system_name": trk.ActiveSystemName,
				"active_game_path":   trk.ActiveGamePath,
			},
			"context_data": map[string]interface{}{
				"core_name":   gameContext.CoreName,
				"game_name":   gameContext.GameName,
				"system_name": gameContext.SystemName,
				"game_path":   gameContext.GamePath,
			},
			"detection_results": map[string]interface{}{
				"is_arcade":       gameContext.SystemName == "Arcade",
				"extraction_test": client.extractArcadeGameName(trk.ActiveCore),
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(debugInfo)

		logger.Info("claude debug: Debug info sent to client")
	}
}

func HandleGetGameContext(logger *service.Logger, cfg *config.UserConfig, trk *tracker.Tracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Only allow GET requests
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Create Claude client
		client := NewClient(&cfg.Claude, logger)

		// Build game context with Claude processing
		gameContext := client.buildGameContext(trk)

		// ✅ IMPROVED: Determine if SAM is actively running the current game
		samActiveForCurrentGame := false
		if client.isSAMActive() {
			samGameName, samSystemName, err := client.parseSAMGameInfo()
			if err == nil {
				samActiveForCurrentGame = client.verifySAMGameMatch(trk, samGameName, samSystemName)
			}
		}

		// Prepare response with clean data + accurate sam_active flag
		response := map[string]interface{}{
			"core_name":    gameContext.CoreName,
			"game_name":    gameContext.GameName,
			"system_name":  gameContext.SystemName,
			"game_path":    gameContext.GamePath,
			"last_started": gameContext.LastStarted,
			"sam_active":   samActiveForCurrentGame, // ✅ IMPROVED: Only true if SAM is running current game
			"timestamp":    time.Now(),
		}

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Encode and send response
		err := json.NewEncoder(w).Encode(response)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			logger.Error("claude game context: failed to encode response: %s", err)
			return
		}

		logger.Info("claude game context: returned context for '%s' (%s) - SAM active for current game: %v",
			gameContext.GameName, gameContext.SystemName, samActiveForCurrentGame)
	}
}

// PDF format generator
func formatPlaylistPDF(games []GameRecommendation, theme string) ([]byte, error) {
	pdf := gofpdf.New("P", "mm", "A4", "")
	pdf.AddPage()

	// Set margins
	pdf.SetMargins(20, 20, 20)

	// Title
	pdf.SetFont("Arial", "B", 18)
	pdf.SetTextColor(0, 0, 0)
	pdf.CellFormat(0, 15, fmt.Sprintf("Claude AI Playlist: %s", theme), "", 1, "C", false, 0, "")
	pdf.Ln(5)

	// Subtitle
	pdf.SetFont("Arial", "", 12)
	pdf.SetTextColor(100, 100, 100)
	pdf.CellFormat(0, 8, fmt.Sprintf("Generated: %s | Games: %d", time.Now().Format("January 2, 2006 at 15:04"), len(games)), "", 1, "C", false, 0, "")
	pdf.Ln(10)

	// Games list
	pdf.SetFont("Arial", "", 11)
	pdf.SetTextColor(0, 0, 0)

	for i, game := range games {
		// Check if we need a new page
		if pdf.GetY() > 250 {
			pdf.AddPage()
		}

		// Game number and name
		pdf.SetFont("Arial", "B", 12)
		pdf.SetTextColor(50, 50, 150)
		pdf.CellFormat(0, 8, fmt.Sprintf("%d. %s", i+1, game.Name), "", 1, "L", false, 0, "")

		// System
		pdf.SetFont("Arial", "", 10)
		pdf.SetTextColor(100, 100, 100)
		pdf.CellFormat(0, 6, fmt.Sprintf("System: %s", game.System), "", 1, "L", false, 0, "")

		// Description (if available)
		if game.Description != "" {
			pdf.SetFont("Arial", "", 10)
			pdf.SetTextColor(0, 0, 0)
			pdf.SetX(25) // Indent

			// Word wrap for long descriptions
			lines := pdf.SplitLines([]byte(fmt.Sprintf("Description: %s", game.Description)), 160)
			for _, line := range lines {
				pdf.CellFormat(0, 5, string(line), "", 1, "L", false, 0, "")
				pdf.SetX(25)
			}
		}

		// Reason (if available)
		if game.Reason != "" {
			pdf.SetFont("Arial", "I", 10)
			pdf.SetTextColor(0, 100, 0)
			pdf.SetX(25) // Indent

			// Word wrap for long reasons
			lines := pdf.SplitLines([]byte(fmt.Sprintf("Why: %s", game.Reason)), 160)
			for _, line := range lines {
				pdf.CellFormat(0, 5, string(line), "", 1, "L", false, 0, "")
				pdf.SetX(25)
			}
		}

		// Path (if available)
		if game.Path != "" {
			pdf.SetFont("Arial", "", 8)
			pdf.SetTextColor(150, 150, 150)
			pdf.SetX(25) // Indent

			// Truncate very long paths
			path := game.Path
			if len(path) > 80 {
				path = "..." + path[len(path)-77:]
			}
			pdf.CellFormat(0, 4, fmt.Sprintf("Path: %s", path), "", 1, "L", false, 0, "")
		}

		pdf.Ln(3) // Space between games
	}

	// Footer
	pdf.SetY(-20)
	pdf.SetFont("Arial", "I", 8)
	pdf.SetTextColor(150, 150, 150)
	pdf.CellFormat(0, 10, "Generated by MiSTer Remote - Claude AI Playlist Generator", "", 0, "C", false, 0, "")

	// Generate PDF bytes
	var buf bytes.Buffer
	err := pdf.Output(&buf)
	if err != nil {
		return nil, fmt.Errorf("failed to generate PDF: %w", err)
	}

	return buf.Bytes(), nil
}

// LaunchSync (.sync) format generator
func formatPlaylistSync(games []GameRecommendation, theme string) string {
	var content strings.Builder

	// Header section
	content.WriteString(fmt.Sprintf("name = Claude AI Playlist: %s\n", theme))
	content.WriteString("author = Claude AI via Remote\n")
	content.WriteString("url = \n") // Empty URL for local use
	content.WriteString(fmt.Sprintf("updated = %s\n\n", time.Now().Format("2006-01-02")))

	// Game sections
	for _, game := range games {
		content.WriteString(fmt.Sprintf("[%s]\n", game.Name))

		// Map system names to LaunchSync format
		systemName := mapSystemToLaunchSync(game.System)
		content.WriteString(fmt.Sprintf("system = %s\n", systemName))

		// Generate match patterns for better ROM finding
		matches := generateMatchPatterns(game.Name)
		for _, match := range matches {
			content.WriteString(fmt.Sprintf("match = %s\n", match))
		}

		content.WriteString("\n")
	}

	return content.String()
}

// Map system names from Claude to LaunchSync format
func mapSystemToLaunchSync(system string) string {
	systemMap := map[string]string{
		"Super Nintendo":     "SNES",
		"Nintendo":           "NES",
		"Sega Genesis":       "Genesis",
		"Game Boy":           "Gameboy",
		"Game Boy Color":     "GBC",
		"Game Boy Advance":   "GBA",
		"PlayStation":        "PSX",
		"Arcade":             "Arcade",
		"Neo Geo":            "NeoGeo",
		"TurboGrafx-16":      "TGFX16",
		"Sega Master System": "SMS",
		"Atari 2600":         "Atari2600",
		"Commodore 64":       "C64",
		"Amiga":              "Amiga",
	}

	if mapped, exists := systemMap[system]; exists {
		return mapped
	}

	// Default fallback - clean the system name
	return strings.ReplaceAll(system, " ", "")
}

// Generate multiple match patterns for better ROM finding
func generateMatchPatterns(gameName string) []string {
	var patterns []string

	// Exact match
	patterns = append(patterns, gameName)

	// Common region variations
	variations := []string{
		gameName + " (USA)",
		gameName + " (World)",
		gameName + " (Europe)",
		gameName + " (Japan)",
		gameName + " (U)",
		gameName + " (W)",
		gameName + " (E)",
		gameName + " (J)",
	}

	patterns = append(patterns, variations...)

	// Remove special characters for fuzzy matching
	cleanName := strings.ReplaceAll(gameName, ":", "")
	cleanName = strings.ReplaceAll(cleanName, "'", "")
	if cleanName != gameName {
		patterns = append(patterns, cleanName)
		patterns = append(patterns, cleanName+" (USA)")
	}

	// Regex pattern for broader matching (prefix with ~)
	if len(gameName) > 5 {
		patterns = append(patterns, fmt.Sprintf("~^%s", regexp.QuoteMeta(gameName)))
	}

	return patterns
}
