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

		// Get games with aggressive timeout (max 8 seconds for scanning)
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

		// Reduce dataset for Claude (max 50 games)
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

		// Add metadata for export functionality
		if response.Error == "" {
			for i := range response.Games {
				response.Games[i].GeneratedAt = time.Now()
				response.Games[i].Theme = response.Theme
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
					System: system.Id,
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
					System: system.Id,
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

func HandleExportPlaylist(logger *service.Logger, cfg *config.UserConfig, trk *tracker.Tracker) http.HandlerFunc {
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

		// ✅ DEBUG: Log the incoming theme
		logger.Info("=== EXPORT DEBUG ===")
		logger.Info("Received theme: '%s'", request.Theme)
		logger.Info("Format: '%s'", format)

		// ✅ DEBUG: Check if theme is detected as active game keyword
		isActiveKeyword := isActiveGameThemeKeyword(request.Theme)
		logger.Info("isActiveGameThemeKeyword result: %v", isActiveKeyword)

		// ✅ NEW: Get active game info if this is an active game-based playlist
		var activeGameName string
		if isActiveKeyword {
			logger.Info("Theme detected as active game keyword - getting game context")
			client := NewClient(&cfg.Claude, logger)
			gameContext := client.buildGameContext(trk)

			// ✅ DEBUG: Log game context details
			logger.Info("Game context - GameName: '%s', SystemName: '%s', CoreName: '%s'",
				gameContext.GameName, gameContext.SystemName, gameContext.CoreName)

			if gameContext.GameName != "" {
				activeGameName = gameContext.GameName
				logger.Info("Active game name set to: '%s'", activeGameName)
			} else {
				logger.Info("WARNING: Game context has empty GameName")
			}
		} else {
			logger.Info("Theme NOT detected as active game keyword")
		}

		var content []byte
		var contentType string
		var filename string

		// ✅ NEW: Generate custom filename based on active game
		filenameTheme := request.Theme
		if activeGameName != "" && isActiveKeyword {
			filenameTheme = fmt.Sprintf("Similar games to %s", activeGameName)
			logger.Info("Custom filename theme: '%s'", filenameTheme)
		} else {
			logger.Info("Using original theme for filename: '%s'", filenameTheme)
		}

		switch format {
		case "txt":
			textContent := formatPlaylistTXT(request.Games, request.Theme, activeGameName)
			content = []byte(textContent)
			contentType = "text/plain"
			filename = fmt.Sprintf("playlist_%s.txt", sanitizeFilename(filenameTheme))
		case "pdf":
			pdfContent, err := formatPlaylistPDF(request.Games, request.Theme, activeGameName)
			if err != nil {
				logger.Error("claude playlist: failed to generate PDF: %s", err)
				http.Error(w, "Failed to generate PDF", http.StatusInternalServerError)
				return
			}
			content = pdfContent
			contentType = "application/pdf"
			filename = fmt.Sprintf("playlist_%s.pdf", sanitizeFilename(filenameTheme))
		case "sync":
			syncContent := formatPlaylistSync(request.Games, request.Theme, activeGameName)
			content = []byte(syncContent)
			contentType = "text/plain"
			filename = fmt.Sprintf("playlist_%s.sync", sanitizeFilename(filenameTheme))
		default:
			http.Error(w, "Unsupported format", http.StatusBadRequest)
			return
		}

		// ✅ DEBUG: Log final filename
		logger.Info("Final filename: '%s'", filename)
		logger.Info("===================")

		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
		w.WriteHeader(http.StatusOK)
		w.Write(content)

		logger.Info("claude playlist: exported %d games as %s format with filename: %s", len(request.Games), format, filename)
	}
}

// ✅ FORMAT: TXT format for playlist
func formatPlaylistTXT(games []GameRecommendation, theme string, activeGameName string) string {
	var content strings.Builder

	// ✅ Enhanced title for active game playlists
	title := theme
	if activeGameName != "" && isActiveGameThemeKeyword(theme) {
		title = fmt.Sprintf("Games similar to %s", activeGameName)
	}

	content.WriteString(fmt.Sprintf("# Claude AI Playlist: %s\n", title))
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
		"games similar to", // This covers "Games similar to [game name]"
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

		// Determine if SAM is actively running the current game
		samActiveForCurrentGame := false
		if client.isSAMActive() {
			samGameName, samSystemName, err := client.parseSAMGameInfo()
			if err == nil {
				samActiveForCurrentGame = client.verifySAMGameMatch(trk, samGameName, samSystemName)
			}
		}

		// Apply display name mapping for Game Info visualization ONLY
		// This converts internal system IDs (like "NeoGeo") to user-friendly names
		// (like "SNK Neo Geo AES & MVS") for better display in the UI
		displaySystemName := mapSystemToDisplayName(gameContext.SystemName)

		// Prepare response with clean data + accurate sam_active flag
		response := map[string]interface{}{
			"core_name":    gameContext.CoreName,
			"game_name":    gameContext.GameName,
			"system_name":  displaySystemName, // ✅ CRITICAL: Use display name for UI
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

		// LOG: Show both internal and display names for debugging
		logger.Info("claude game context: returned context - Game: '%s', Internal System: '%s', Display System: '%s'",
			gameContext.GameName, gameContext.SystemName, displaySystemName)
	}
}

// PDF format generator
func formatPlaylistPDF(games []GameRecommendation, theme string, activeGameName string) ([]byte, error) {
	pdf := gofpdf.New("P", "mm", "A4", "")
	pdf.AddPage()

	// Set larger margins to prevent text truncation
	pdf.SetMargins(25, 25, 25) // Increased from 20 to 25

	// Enhanced title for active game playlists
	title := theme
	if activeGameName != "" && isActiveGameThemeKeyword(theme) {
		title = fmt.Sprintf("Games similar to %s", activeGameName)
	}

	// Use smaller font size and check title length
	pdf.SetFont("Arial", "B", 16) // Reduced from 18 to 16
	pdf.SetTextColor(0, 0, 0)

	// Split long titles into multiple lines if needed
	fullTitle := fmt.Sprintf("Claude AI Playlist: %s", title)

	// Check if title is too long (more than 60 characters)
	if len(fullTitle) > 60 {
		// Split into two lines
		pdf.CellFormat(0, 8, "Claude AI Playlist:", "", 1, "C", false, 0, "")
		pdf.SetFont("Arial", "B", 14) // Slightly smaller for subtitle
		pdf.CellFormat(0, 8, title, "", 1, "C", false, 0, "")
	} else {
		// Single line title
		pdf.CellFormat(0, 10, fullTitle, "", 1, "C", false, 0, "")
	}

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
func formatPlaylistSync(games []GameRecommendation, theme string, activeGameName string) string {
	var content strings.Builder

	// Enhanced name for active game playlists
	name := theme
	if activeGameName != "" && isActiveGameThemeKeyword(theme) {
		name = fmt.Sprintf("Games similar to %s", activeGameName)
	}

	// Header section
	content.WriteString(fmt.Sprintf("name = Claude AI Playlist: %s\n", name))
	content.WriteString("author = Claude AI via Remote\n")
	content.WriteString("url = \n") // Empty URL for local use
	content.WriteString(fmt.Sprintf("updated = %s\n\n", time.Now().Format("2006-01-02")))

	// Game sections
	for _, game := range games {
		content.WriteString(fmt.Sprintf("[%s]\n", game.Name))

		// Map system names to LaunchSync format
		systemName := mapSystemToLaunchSync(game.System)
		content.WriteString(fmt.Sprintf("system = %s\n", systemName))

		// Generate match patterns using exact file names
		matches := generateMatchPatterns(game)
		for _, match := range matches {
			content.WriteString(fmt.Sprintf("match = %s\n", match))
		}

		content.WriteString("\n")
	}

	return content.String()
}

// Map system names from Claude to LaunchSync format
func mapSystemToLaunchSync(system string) string {
	// SystemName from game context is already the correct core ID
	// The SystemName comes from inferSystemFromPath() which extracts the folder name
	// This is exactly what LaunchSync expects, so use it directly without any mapping

	// Clean up any whitespace
	system = strings.TrimSpace(system)

	// If the system is empty, return as-is
	if system == "" {
		return system
	}

	// The SystemName is already the correct core ID
	// Examples:
	// - "NEOGEO" stays "NEOGEO"
	// - "NES" stays "NES"
	// - "Genesis" stays "Genesis"
	// - "Arcade" stays "Arcade"

	return system
}

func generateMatchPatterns(game GameRecommendation) []string {
	var patterns []string

	// ✅ PRIMARY: Use exact filename from path (without extension)
	if game.Path != "" {
		// Extract filename from full path
		filename := filepath.Base(game.Path)

		// Remove extension to get exact ROM name
		nameWithoutExt := strings.TrimSuffix(filename, filepath.Ext(filename))

		// This is the EXACT match that LaunchSync needs
		patterns = append(patterns, nameWithoutExt)

		// ✅ SECONDARY: Add regex pattern for fuzzy matching if needed
		if len(nameWithoutExt) > 5 {
			patterns = append(patterns, fmt.Sprintf("~^%s", regexp.QuoteMeta(nameWithoutExt)))
		}
	} else {
		// ✅ FALLBACK: If no path available, use game name as-is
		patterns = append(patterns, game.Name)

		// Add basic region variants only as fallback
		commonVariants := []string{
			game.Name + " (USA)",
			game.Name + " (World)",
			game.Name + " (Europe)",
		}
		patterns = append(patterns, commonVariants...)

		// Add regex pattern for broader matching
		if len(game.Name) > 5 {
			patterns = append(patterns, fmt.Sprintf("~^%s", regexp.QuoteMeta(game.Name)))
		}
	}

	return patterns
}

// mapSystemToDisplayName converts internal system IDs to user-friendly display names
// based on the official MiSTer documentation pages and actual system folders
func mapSystemToDisplayName(systemID string) string {
	// System display name mapping based on:
	// https://mister-devel.github.io/MkDocs_MiSTer/cores/console/
	// https://mister-devel.github.io/MkDocs_MiSTer/cores/computer/
	// + actual system folders found in user's MiSTer
	displayNameMap := map[string]string{
		// Console Systems
		"S32X":               "Sega 32X",
		"AY-3-8500":          "\"Pong-on-a-chip\"",
		"AVision":            "Entex Adventure Vision",
		"Arcadia":            "Emerson Arcadia 2001",
		"Astrocade":          "Bally Astrocade",
		"Atari2600":          "Atari 2600",
		"Atari5200":          "Atari 5200 SuperSystem",
		"ATARI5200":          "Atari 5200 SuperSystem", // Alt capitalization
		"Atari7800":          "Atari 7800 ProSystem / Atari 2600",
		"ATARI7800":          "Atari 7800 ProSystem / Atari 2600", // Alt capitalization
		"AtariLynx":          "Atari Lynx",
		"BBCBridgeCompanion": "BBC Bridge Companion",
		"Casio_PV-1000":      "Casio PV-1000",
		"ChannelF":           "Fairchild Channel F",
		"ColecoVision":       "ColecoVision / Sega SG-1000",
		"Coleco":             "ColecoVision",
		"CreatiVision":       "VTech CreatiVision / Dick Smith Wizzard",
		"Gamate":             "Bit Corp Gamate",
		"GBA":                "Nintendo Game Boy Advance",
		"GBA2P":              "2x Game Boy Advance (2-Player)",
		"GameBoy":            "Nintendo Game Boy",
		"GAMEBOY":            "Nintendo Game Boy", // Alt capitalization
		"GBC":                "Nintendo Game Boy Color",
		"Gameboy2P":          "2x Game Boy (2-Player)",
		"GAMEBOY2P":          "2x Game Boy (2-Player)", // Alt capitalization
		"GameGear":           "Sega Game Gear",
		"GameNWatch":         "Nintendo Game & Watch Handheld Devices",
		"Intv":               "Mattel Intellivision",
		"Intellivision":      "Mattel Intellivision",
		"MyVision":           "Nichibutsu My Vision",
		"MegaCD":             "Sega CD / Sega Mega-CD",
		"MegaDrive":          "Sega Mega Drive / Sega Genesis",
		"Genesis":            "Sega Mega Drive / Sega Genesis", // Alternative name
		"MegaDuck":           "Watara Mega Duck",
		"N64":                "Nintendo 64",
		"NES":                "Nintendo Entertainment System / Famicom Disk System / NSF Music Player",
		"FDS":                "Nintendo Famicom Disk System",
		"NeoGeo":             "SNK Neo Geo AES & MVS",
		"NEOGEO":             "SNK Neo Geo AES & MVS", // Alt capitalization
		"NeoGeo-CD":          "SNK Neo Geo CD",
		"NeoGeoPocket":       "SNK Neo Geo Pocket / Neo Geo Pocket Color",
		"Odyssey2":           "Magnavox Odyssey 2 / Philips Odyssey 2 / Philips Videopac G7000",
		"ODYSSEY2":           "Magnavox Odyssey 2 / Philips Odyssey 2 / Philips Videopac G7000", // Alt capitalization
		"PokemonMini":        "Pokémon Mini",
		"PSX":                "Sony Playstation",
		"Saturn":             "Sega Saturn",
		"SGB":                "Nintendo Super Game Boy and Super Game Boy 2",
		"SG1000":             "Sega SG-1000",
		"SG-1000":            "Sega SG-1000",
		"SMS":                "Sega Master System / Sega Game Gear / Sega SG-1000",
		"SNES":               "Super Nintendo Entertainment System / Nintendo Satellaview / SPC Music Player",
		"SuperGrafx":         "NEC SuperGrafx",
		"Super_Vision_8000":  "Bandai Super Vision 8000",
		"SuperVision8000":    "Bandai Super Vision 8000", // Alt format
		"SuperVision":        "Watara SuperVision",
		"TurboGrafx16":       "NEC TurboGrafx-16 / PC Engine / CD-ROM² / Super CD-ROM² / Duo / TurboDuo / SuperGrafx / Arcade Card",
		"TGFX16":             "NEC TurboGrafx-16 / PC Engine / CD-ROM² / Super CD-ROM² / Duo / TurboDuo / SuperGrafx / Arcade Card", // Alternative name
		"TGFX16-CD":          "NEC TurboGrafx-16 CD / PC Engine CD",
		"VC4000":             "Interton VC4000 / Acetronic MPU-1000 / Occitane OC2000",
		"Vectrex":            "Vectrex",
		"VECTREX":            "Vectrex", // Alt capitalization
		"WonderSwan":         "Bandai WonderSwan / WonderSwan Color / SwanCrystal",
		"WonderSwanColor":    "Bandai WonderSwan Color",
		"PocketChallengeV2":  "Benesse Pocket Challenge V2",
		"EpochGalaxyII":      "Epoch Galaxy II",
		"CD-i":               "Philips CD-i",
		"SCV":                "Epoch Super Cassette Vision",

		// Computer Systems
		"AcornAtom":      "Acorn Atom",
		"AcornElectron":  "Acorn Electron",
		"Adam":           "Coleco Adam",
		"ColecoAdam":     "Coleco Adam", // Alternative
		"AliceMC10":      "Matra & Hachette Ordinateur Alice (TRS-80 MC-10 Clone)",
		"Altair8800":     "MITS Altair 8800",
		"AmstradPCW":     "Amstrad PCW",
		"Amstrad PCW":    "Amstrad PCW", // Alt format with space
		"Amstrad":        "Amstrad CPC 6128",
		"ao486":          "486DX33 (No FPU) compatible",
		"AO486":          "486DX33 (No FPU) compatible", // Alternative name
		"Apogee":         "Apogee BK-01 / Radio-86RK",
		"APOGEE":         "Apogee BK-01 / Radio-86RK", // Alt capitalization
		"Apple-II":       "Apple IIe",
		"Apple-I":        "Apple I",
		"APPLE-I":        "Apple I", // Alt capitalization
		"Aquarius":       "Mattel Aquarius",
		"AQUARIUS":       "Mattel Aquarius", // Alt capitalization
		"Archie":         "Acorn Archimedes",
		"ARCHIE":         "Acorn Archimedes", // Alt capitalization
		"Atari800":       "Atari 800 / 800XL / 65XE / 130XE",
		"ATARI800":       "Atari 800 / 800XL / 65XE / 130XE", // Alt capitalization
		"AtariST":        "Atari ST / STe",
		"BBCMicro":       "BBC Micro B / Master 128K",
		"BK0011M":        "Elektronika BK (BK-0011M CPU)",
		"C16":            "Commodore C16 / Plus/4",
		"C64":            "Commodore 64 / Games System / 128",
		"C128":           "Commodore 128",
		"Casio_PV-2000":  "Casio PV-2000",
		"Chip8":          "CHIP-8 by Joseph Weisbecker",
		"CoCo2":          "Tandy Color Computer 2 / Dragon 32",
		"CoCo3":          "Tandy Color Computer 3",
		"COCO3":          "Tandy Color Computer 3", // Alt capitalization
		"EDSAC":          "EDSAC",
		"EG2000":         "EACA EG2000 Colour Genie",
		"eg2000":         "EACA EG2000 Colour Genie", // Alt capitalization
		"Galaksija":      "Galaksija by Voja Antonić",
		"Homelab":        "Compukit Homelab",
		"Interact":       "Interact Home Computer",
		"Jupiter":        "Jupiter Ace",
		"Laser":          "Vtech Laser 310",
		"Laser310":       "Vtech Laser 310",
		"Lynx48":         "Camputers Lynx 48k, 96k",
		"MacPlus":        "Macintosh Plus",
		"MACPLUS":        "Macintosh Plus", // Alt capitalization
		"Minimig-AGA":    "Commodore Amiga 500 / 600 / 1200 / 4000 / CD32",
		"Amiga":          "Commodore Amiga 500 / 600 / 1200 / 4000 / CD32", // Common reference
		"MSX1":           "Microsoft MSX1",
		"MSX":            "Microsoft MSX / MSX2 / Plus / MSX3 / TurboR",
		"MultiComp":      "Grant Searle's MultiComp",
		"OndraSPO186":    "Tesla Ondra SPO-186",
		"Ondra_SPO186":   "Tesla Ondra SPO-186", // Alt format
		"Orao":           "PEL Varaždin Orao / Eagle",
		"ORAO":           "PEL Varaždin Orao / Eagle", // Alt capitalization
		"Oric":           "Tangerine Oric / Oric-1",
		"PCXT":           "IBM PC/XT",
		"PC88":           "NEC PC8801 MKII SR",
		"PC8801":         "NEC PC8801 MKII SR", // Alt name
		"PDP1":           "DEC PDP-1",
		"PET2001":        "Commodore PET 2001",
		"PMD85":          "Tesla PMD 85",
		"QL":             "Sinclair QL",
		"RX-78":          "Bandai RX-78",
		"RX78":           "Bandai RX-78", // Alt format
		"SAM-Coupe":      "Miles Gordon Technology SAM Coupé",
		"SAMCOUPE":       "Miles Gordon Technology SAM Coupé", // Alt format
		"SharpMZ":        "Sharp MZ",
		"SordM5":         "Sord M5",
		"Sord M5":        "Sord M5", // Alt format with space
		"Specialist":     "Specialist / Специалист",
		"SPMX":           "Specialist / Специалист", // Alt name
		"SVI328":         "Spectravideo SV-328",
		"TatungEinstein": "Tatung Einstein TC01 & 256",
		"TI-99_4A":       "Texas Instruments TI-99/4A",
		"TomyTutor":      "Tomy Tutor, Pyuta, and Pyuta Jr.",
		"TomyScramble":   "Tomy Tutor Scramble",
		"TRS-80":         "Radio Shack / Tandy TRS-80 Micro Computer System / Model I",
		"TSConf":         "TSConf (ZX-Evolution Improvement)",
		"UK101":          "Compukit UK101",
		"Vector-06C":     "Vector-06C / Вектор-06Ц",
		"VECTOR06":       "Vector-06C / Вектор-06Ц", // Alt format
		"VIC20":          "Commodore VIC-20",
		"VT52":           "DEC VT52 Terminal",
		"X68000":         "Sharp X68000",
		"ZX-Spectrum":    "Sinclair ZX Spectrum",
		"Spectrum":       "Sinclair ZX Spectrum",     // Common reference
		"zx48":           "Sinclair ZX Spectrum 48K", // Specific variant
		"ZX81":           "Sinclair ZX80 / ZX81",
		"ZXNext":         "ZX Spectrum Next",

		// Special/Utility Systems
		"Arcade":  "Arcade", // Keep as-is for arcade games
		"mame":    "MAME Arcade",
		"hbmame":  "HBMAME (Homebrew MAME)",
		"MEMTEST": "Memory Test Utility",
	}

	// Return mapped name if available, otherwise return original ID
	if displayName, exists := displayNameMap[systemID]; exists {
		return displayName
	}

	// Fallback: return the original system ID
	return systemID
}
