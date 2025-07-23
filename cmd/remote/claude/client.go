package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/wizzomafizzo/mrext/pkg/config"
	"github.com/wizzomafizzo/mrext/pkg/service"
	"github.com/wizzomafizzo/mrext/pkg/tracker"
)

const (
	anthropicAPIURL = "https://api.anthropic.com/v1/messages"
	maxTokens       = 1024
	userAgent       = "MiSTer-Remote-Claude/1.0"
)

// SAMGameInfo holds parsed information from SAM's game file
type SAMGameInfo struct {
	GameName   string
	CoreName   string
	SystemName string
}
type Client struct {
	config      *config.ClaudeConfig
	logger      *service.Logger
	httpClient  *http.Client
	rateLimiter *RateLimiter
	sessions    map[string]*ChatSession
	sessionMux  sync.RWMutex
}

// NewClient creates a new Claude AI client
func NewClient(cfg *config.ClaudeConfig, logger *service.Logger) *Client {
	return &Client{
		config: cfg,
		logger: logger,
		httpClient: &http.Client{
			Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second,
		},
		rateLimiter: NewRateLimiter(cfg.MaxRequestsPerHour, time.Hour),
		sessions:    make(map[string]*ChatSession),
	}
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(maxRequests int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		requests:    make([]time.Time, 0),
		maxRequests: maxRequests,
		window:      window,
	}
}

// Allow checks if a request is allowed under rate limiting
func (r *RateLimiter) Allow() bool {
	now := time.Now()

	// Remove old requests outside the window
	cutoff := now.Add(-r.window)
	validRequests := make([]time.Time, 0)
	for _, req := range r.requests {
		if req.After(cutoff) {
			validRequests = append(validRequests, req)
		}
	}
	r.requests = validRequests

	// Check if we can make another request
	if len(r.requests) >= r.maxRequests {
		return false
	}

	// Add current request
	r.requests = append(r.requests, now)
	return true
}

// SendMessage sends a message to Claude and returns the response
func (c *Client) SendMessage(ctx context.Context, message string, gameContext *GameContext, sessionID string) (*ChatResponse, error) {
	if !c.config.Enabled {
		return nil, fmt.Errorf("claude is disabled in configuration")
	}

	if !c.rateLimiter.Allow() {
		c.logger.Info("claude rate limit exceeded, request denied")
		return &ChatResponse{
			Error:     "Rate limit exceeded. Please try again later.",
			Timestamp: time.Now(),
		}, nil
	}

	prompt := c.buildPrompt(message, gameContext)
	session := c.getOrCreateSession(sessionID)

	// Add user message to session
	session.Messages = append(session.Messages, Message{
		Role:    "user",
		Content: prompt,
	})

	// Trim session history if too long
	c.trimSessionHistory(session)

	// Prepare API request
	apiRequest := AnthropicRequest{
		Model:     c.config.Model,
		MaxTokens: maxTokens,
		Messages:  session.Messages,
	}

	// Call Anthropic API
	apiResponse, err := c.callAnthropicAPI(ctx, &apiRequest)
	if err != nil {
		c.logger.Error("claude api call failed: %s", err)
		return &ChatResponse{
			Error:     "Failed to communicate with Claude. Please try again.",
			Timestamp: time.Now(),
			Context:   gameContext,
		}, nil
	}

	// Extract response text
	responseText := ""
	if len(apiResponse.Content) > 0 {
		responseText = apiResponse.Content[0].Text
	}

	// Add assistant response to session
	session.Messages = append(session.Messages, Message{
		Role:    "assistant",
		Content: responseText,
	})
	session.Updated = time.Now()

	c.logger.Info("claude response generated successfully (tokens: %d/%d)",
		apiResponse.Usage.InputTokens, apiResponse.Usage.OutputTokens)

	return &ChatResponse{
		Content:   responseText,
		Timestamp: time.Now(),
		Context:   gameContext,
	}, nil
}

// GenerateSuggestions creates game tips and suggestions
func (c *Client) GenerateSuggestions(ctx context.Context, trk *tracker.Tracker) (*SuggestionsResponse, error) {
	if !c.config.Enabled || !c.config.AutoSuggestions {
		return &SuggestionsResponse{
			Suggestions: []string{},
			Timestamp:   time.Now(),
		}, nil
	}

	gameContext := c.BuildGameContext(trk)
	if gameContext.GameName == "" {
		return &SuggestionsResponse{
			Suggestions: []string{"No game currently running"},
			Timestamp:   time.Now(),
		}, nil
	}

	prompt := fmt.Sprintf(`Generate 3 brief, helpful suggestions for someone playing "%s" on the %s system. 
Focus on tips, strategies, or interesting facts. Keep each suggestion under 50 words.
Format as a simple list without numbers or bullets.`,
		gameContext.GameName, gameContext.SystemName)

	response, err := c.SendMessage(ctx, prompt, gameContext, "suggestions")
	if err != nil {
		return &SuggestionsResponse{
			Error:     "Failed to generate suggestions",
			Context:   gameContext,
			Timestamp: time.Now(),
		}, nil
	}

	if response.Error != "" {
		return &SuggestionsResponse{
			Error:     response.Error,
			Context:   gameContext,
			Timestamp: time.Now(),
		}, nil
	}

	suggestions := c.parseSuggestions(response.Content)

	return &SuggestionsResponse{
		Suggestions: suggestions,
		Context:     gameContext,
		Timestamp:   time.Now(),
	}, nil
}

// GeneratePlaylist creates themed game playlists
func (c *Client) GeneratePlaylist(ctx context.Context, request *PlaylistRequest) (*PlaylistResponse, error) {
	if !c.config.Enabled {
		return &PlaylistResponse{
			Error:     "Claude is disabled",
			Timestamp: time.Now(),
		}, nil
	}

	prompt := c.buildPlaylistPrompt(request)

	response, err := c.SendMessage(ctx, prompt, nil, "playlist")
	if err != nil {
		return &PlaylistResponse{
			Error:     "Failed to generate playlist",
			Theme:     request.Theme,
			Timestamp: time.Now(),
		}, nil
	}

	if response.Error != "" {
		return &PlaylistResponse{
			Error:     response.Error,
			Theme:     request.Theme,
			Timestamp: time.Now(),
		}, nil
	}

	// Parse and validate game recommendations
	games := c.parseGameRecommendations(response.Content, request.GameCount, request.InstalledGames)

	return &PlaylistResponse{
		Games:     games,
		Theme:     request.Theme,
		Timestamp: time.Now(),
	}, nil
}

// buildPrompt creates a prompt with optional game context
func (c *Client) buildPrompt(message string, gameContext *GameContext) string {
	if gameContext == nil || gameContext.GameName == "" {
		return fmt.Sprintf("You are Claude, an AI assistant integrated into MiSTer FPGA Remote. "+
			"Help the user with their question: %s", message)
	}

	return fmt.Sprintf(`You are Claude, an AI assistant integrated into MiSTer FPGA Remote.
Current context:
- Game: %s
- System: %s
- Core: %s

The user is currently playing this game and asks: %s

Provide helpful, relevant advice based on the game context.`,
		gameContext.GameName, gameContext.SystemName, gameContext.CoreName, message)
}

// buildGameContext extracts current game information from tracker
func (c *Client) BuildGameContext(trk *tracker.Tracker) *GameContext {
	// ✅ EXTENSIVE DEBUG LOGGING
	c.logger.Info("claude debug: === BUILDING GAME CONTEXT ===")
	c.logger.Info("claude debug: ActiveCore = '%s'", trk.ActiveCore)
	c.logger.Info("claude debug: ActiveGameName = '%s'", trk.ActiveGameName)
	c.logger.Info("claude debug: ActiveSystemName = '%s'", trk.ActiveSystemName)
	c.logger.Info("claude debug: ActiveGamePath = '%s'", trk.ActiveGamePath)
	c.logger.Info("claude debug: ActiveGame = '%s'", trk.ActiveGame)

	context := &GameContext{
		CoreName:    trk.ActiveCore,
		GameName:    trk.ActiveGameName,
		SystemName:  trk.ActiveSystemName,
		GamePath:    trk.ActiveGamePath,
		LastStarted: time.Now(),
	}

	// ✅ NEW LOGIC: Check if SAM is active
	if samInfo := c.checkSAMStatus(); samInfo != nil {
		c.logger.Info("claude debug: SAM detected, using SAM game info")
		context.GameName = samInfo.GameName
		context.CoreName = samInfo.CoreName
		context.SystemName = samInfo.SystemName
		return context
	}

	// ✅ CRITICAL: Only override if we don't have good data from tracker
	if context.CoreName != "" {
		c.logger.Info("claude debug: Checking arcade detection for core '%s'", context.CoreName)

		// If tracker already detected arcade properly, trust it!
		if context.SystemName == "Arcade" && context.GameName != "" && context.GameName != context.CoreName {
			c.logger.Info("claude debug: ✅ TRUSTING TRACKER: Arcade game '%s' already detected correctly", context.GameName)
			// Don't override - tracker has the right info
			return context
		}

		// If tracker says it's arcade but no game name, or game name == core name
		if context.SystemName == "Arcade" && (context.GameName == "" || context.GameName == context.CoreName) {
			c.logger.Info("claude debug: Tracker detected arcade but no specific game name, trying extraction...")
			if arcadeName := c.extractArcadeGameName(context.CoreName); arcadeName != "" {
				context.GameName = arcadeName
				c.logger.Info("claude debug: ✅ EXTRACTION SUCCESS: '%s' -> '%s'", context.CoreName, arcadeName)
			} else {
				c.logger.Warn("claude debug: ❌ EXTRACTION FAILED for core '%s'", context.CoreName)
				context.GameName = context.CoreName // fallback
			}
			context.SystemName = "Arcade"
			context.GamePath = ""
			return context
		}

		// If tracker doesn't think it's arcade, but maybe it is
		if context.SystemName != "Arcade" {
			if arcadeName := c.extractArcadeGameName(context.CoreName); arcadeName != "" {
				c.logger.Info("claude debug: ✅ OVERRIDE: Detected arcade core '%s' -> '%s' (tracker missed it)", context.CoreName, arcadeName)
				context.GameName = arcadeName
				context.SystemName = "Arcade"
				context.GamePath = ""
			} else {
				c.logger.Info("claude debug: ✅ NON-ARCADE: Core '%s' is not arcade, keeping tracker data", context.CoreName)
			}
		}
	}

	c.logger.Info("claude debug: === FINAL CONTEXT ===")
	c.logger.Info("claude debug: Final GameName = '%s'", context.GameName)
	c.logger.Info("claude debug: Final SystemName = '%s'", context.SystemName)
	c.logger.Info("claude debug: Final CoreName = '%s'", context.CoreName)

	return context
}

// Enhanced SAM detection with obsolete file detection
func (c *Client) checkSAMStatus() *SAMGameInfo {
	samFile := "/tmp/SAM_Game.txt"

	// Read SAM file
	data, err := os.ReadFile(samFile)
	if err != nil {
		c.logger.Info("claude debug: SAM file not found or error reading: %s", err)
		return nil
	}

	samGameText := strings.TrimSpace(string(data))
	c.logger.Info("claude debug: SAM file content: '%s'", samGameText)

	if samGameText == "" {
		c.logger.Info("claude debug: SAM file is empty")
		return nil
	}

	// ✅ NEW: Check if SAM file is obsolete
	if c.isSAMFileObsolete(samFile) {
		c.logger.Info("claude debug: SAM file appears to be obsolete - ignoring")
		return nil
	}

	samInfo := c.parseSAMGameInfo(samGameText)
	if samInfo != nil {
		c.logger.Info("claude debug: SAM parsed successfully - Game: '%s', Core: '%s', System: '%s'",
			samInfo.GameName, samInfo.CoreName, samInfo.SystemName)
	} else {
		c.logger.Info("claude debug: SAM parsing failed for content: '%s'", samGameText)
	}

	return samInfo
}

// ✅ NEW: Check if SAM file is obsolete (multiple verification methods)
func (c *Client) isSAMFileObsolete(samFile string) bool {
	// Method 1: Check file age
	if c.isSAMFileOld(samFile) {
		c.logger.Info("claude debug: SAM file is too old (>5 minutes)")
		return true
	}

	// Method 2: Check if SAM process is running
	if !c.isSAMProcessRunning() {
		c.logger.Info("claude debug: SAM process not detected")
		return true
	}

	c.logger.Info("claude debug: SAM file appears to be current and valid")
	return false
}

// ✅ NEW: Check if SAM file is older than 5 minutes
func (c *Client) isSAMFileOld(samFile string) bool {
	stat, err := os.Stat(samFile)
	if err != nil {
		c.logger.Info("claude debug: Cannot stat SAM file: %s", err)
		return true // Treat errors as obsolete
	}

	age := time.Since(stat.ModTime())
	maxAge := 5 * time.Minute

	c.logger.Info("claude debug: SAM file age: %v (max allowed: %v)", age, maxAge)
	return age > maxAge
}

// ✅ NEW: Check if SAM process is actually running
func (c *Client) isSAMProcessRunning() bool {
	// Check for common SAM process patterns
	processes := []string{"MiSTer_SAM", "sam", "attract"}

	for _, proc := range processes {
		cmd := exec.Command("pgrep", "-f", proc)
		if err := cmd.Run(); err == nil {
			c.logger.Info("claude debug: Found SAM-related process: %s", proc)
			return true
		}
	}

	// Alternative: Check for SAM script execution
	cmd := exec.Command("pgrep", "-f", "MiSTer_SAM_on.sh")
	if err := cmd.Run(); err == nil {
		c.logger.Info("claude debug: Found SAM script process")
		return true
	}

	c.logger.Info("claude debug: No SAM processes detected")
	return false
}

// Enhanced parsing with detailed logging
func (c *Client) parseSAMGameInfo(samText string) *SAMGameInfo {
	c.logger.Info("claude debug: === PARSING SAM INFO ===")
	c.logger.Info("claude debug: Input text: '%s'", samText)

	// Look for last opening parenthesis
	idx := strings.LastIndex(samText, " (")
	if idx == -1 {
		c.logger.Info("claude debug: No opening parenthesis found")
		return nil
	}

	gameName := samText[:idx]
	coreInfo := samText[idx+2:] // Skip " ("

	c.logger.Info("claude debug: Extracted game name: '%s'", gameName)
	c.logger.Info("claude debug: Core info section: '%s'", coreInfo)

	// Look for closing parenthesis
	endIdx := strings.Index(coreInfo, ")")
	if endIdx == -1 {
		c.logger.Info("claude debug: No closing parenthesis found")
		return nil
	}

	coreName := coreInfo[:endIdx]
	c.logger.Info("claude debug: Extracted core name: '%s'", coreName)

	// Map core to system
	systemName := c.mapCoreToSystem(coreName)
	c.logger.Info("claude debug: Mapped system name: '%s'", systemName)

	return &SAMGameInfo{
		GameName:   gameName,
		CoreName:   coreName,
		SystemName: systemName,
	}
}

// Enhanced mapping with logging
func (c *Client) mapCoreToSystem(coreName string) string {
	systemMap := map[string]string{
		"atari5200": "Atari 5200",
		"atari2600": "Atari 2600",
		"atari7800": "Atari 7800",
		"nes":       "Nintendo Entertainment System",
		"snes":      "Super Nintendo",
		"genesis":   "Sega Genesis",
		"megacd":    "Sega CD",
		"s32x":      "Sega 32X",
		"arcade":    "Arcade",
		"neogeo":    "Neo Geo",
		"cps1":      "Capcom CPS-1",
		"cps2":      "Capcom CPS-2",
		"gb":        "Game Boy",
		"gbc":       "Game Boy Color",
		"gba":       "Game Boy Advance",
		"psx":       "PlayStation",
		"saturn":    "Sega Saturn",
		"n64":       "Nintendo 64",
		"tgfx16":    "TurboGrafx-16",
		"tgfx16cd":  "TurboGrafx-CD",
		"ao486":     "PC (486)",
		"amiga":     "Commodore Amiga",
		"c64":       "Commodore 64",
	}

	if systemName, exists := systemMap[coreName]; exists {
		c.logger.Info("claude debug: Core '%s' mapped to system '%s'", coreName, systemName)
		return systemName
	}

	c.logger.Info("claude debug: Core '%s' not found in map, using as-is", coreName)
	return coreName // Fallback to core name
}

func HandleDebugSAM(logger *service.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		samData, _ := os.ReadFile("/tmp/SAM_Game.txt")
		activeData, _ := os.ReadFile("/tmp/ACTIVEGAME")

		debug := map[string]interface{}{
			"sam_game":    strings.TrimSpace(string(samData)),
			"active_game": strings.TrimSpace(string(activeData)),
			"sam_active":  len(samData) > 0,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(debug)
	}
}

// buildPlaylistPromptGeneric provides fallback for when no games are provided
func (c *Client) buildPlaylistPromptGeneric(request *PlaylistRequest) string {
	prompt := fmt.Sprintf("Generate exactly %d game recommendations for the theme: %s\n",
		request.GameCount, request.Theme)

	if len(request.Systems) > 0 {
		prompt += fmt.Sprintf("Focus on these systems: %v\n", request.Systems)
	}

	if request.Preferences != "" {
		prompt += fmt.Sprintf("User preferences: %s\n", request.Preferences)
	}

	prompt += `
RANDOMIZATION REQUIREMENT:
- Do NOT list games in alphabetical order
- ACTIVELY AVOID alphabetical patterns (A, B, C games)
- Mix different starting letters randomly in your recommendations
- Prioritize theme relevance over alphabetical ordering
- Ensure variety in the first letters of recommended games (avoid clustering around A-D)

Format each game as: Game Name | System | Brief description | Why recommended`
	return prompt
}

// ✅ IMPROVED: parseGameRecommendations with anti-bias fuzzy matching
func (c *Client) parseGameRecommendations(content string, count int, installedGames []InstalledGame) []GameRecommendation {
	c.logger.Info("=== CLAUDE'S RAW RESPONSE ===")
	c.logger.Info("Response length: %d characters", len(content))
	c.logger.Info("Full response: %s", content)
	c.logger.Info("==============================")
	recommendations := make([]GameRecommendation, 0, count)

	// Create fast lookup map of installed games
	installedMap := make(map[string]InstalledGame)
	// ✅ CRITICAL: Also create a randomized slice for unbiased fuzzy matching
	gamesList := make([]InstalledGame, len(installedGames))
	copy(gamesList, installedGames)

	// Randomize the games list for fuzzy matching to prevent alphabetical bias
	baseTime := time.Now().UnixNano() + 7777 // Different seed
	for i := len(gamesList) - 1; i > 0; i-- {
		seed := baseTime + int64(i*3000) + int64(len(gamesList)*300)
		j := int(seed) % (i + 1)
		if j < 0 {
			j = -j
		}
		gamesList[i], gamesList[j] = gamesList[j], gamesList[i]
	}

	// Build the lookup map
	for _, game := range installedGames {
		// Normalize name for search
		key := strings.ToLower(strings.TrimSpace(game.Name))
		installedMap[key] = game
	}

	// Split by double newlines first, then single newlines as backup
	var lines []string
	if strings.Contains(content, "\n\n") {
		parts := strings.Split(content, "\n\n")
		// Take each part and split by single newlines too
		for _, part := range parts {
			subLines := strings.Split(strings.TrimSpace(part), "\n")
			lines = append(lines, subLines...)
		}
	} else {
		lines = strings.Split(strings.TrimSpace(content), "\n")
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines or lines that don't contain pipes
		if line == "" || !strings.Contains(line, "|") {
			continue
		}

		// Skip lines that look like headers or intro text
		if strings.Contains(strings.ToLower(line), "recommendations") ||
			strings.Contains(strings.ToLower(line), "here are") {
			continue
		}

		// Split by pipe character
		parts := strings.Split(line, "|")

		// Should have exactly 4 parts: Name | System | Description | Reason
		if len(parts) != 4 {
			continue
		}

		// Clean up each part
		name := strings.TrimSpace(parts[0])
		system := strings.TrimSpace(parts[1])
		description := strings.TrimSpace(parts[2])
		reason := strings.TrimSpace(parts[3])

		// Validate that we have meaningful content
		if len(name) < 2 || len(system) < 2 || len(description) < 10 {
			continue
		}

		// ✅ NEW: Validate game exists in user's collection (only if installed games provided)
		var installedGame InstalledGame
		var gameFound bool = true

		if len(installedGames) > 0 {
			normalizedName := strings.ToLower(strings.TrimSpace(name))
			installedGame, gameFound = installedMap[normalizedName]

			if !gameFound {
				// ✅ FIXED: Use randomized slice instead of map iteration to prevent bias
				for _, installed := range gamesList {
					installedName := strings.ToLower(strings.TrimSpace(installed.Name))
					if strings.Contains(installedName, normalizedName) ||
						strings.Contains(normalizedName, installedName) {
						installedGame = installed
						gameFound = true
						break
					}
				}
				if !gameFound {
					c.logger.Warn("claude playlist: game '%s' not found in collection, skipping", name)
					continue
				}
			}

			// Use actual game path from collection
			if gameFound {
				name = installedGame.Name     // Use exact name from collection
				system = installedGame.System // Use exact system name
			}
		} else {
			// No validation possible, use Claude's data as-is
			installedGame = InstalledGame{
				Name:   name,
				System: system,
			}
		}

		recommendation := GameRecommendation{
			Name:        name,
			System:      system,
			Path:        installedGame.Path,
			Description: description,
			Reason:      reason,
			GeneratedAt: time.Now(),
		}

		recommendations = append(recommendations, recommendation)

		// Stop if we have enough recommendations
		if len(recommendations) >= count {
			break
		}
	}

	// ✅ BALANCE CHECK: If multiple systems were requested, try to balance recommendations
	if len(recommendations) > 1 {
		recommendations = c.verifySystemBalance(recommendations, []string{}, count)
	}
	c.logger.Info("=== FINAL PARSED RECOMMENDATIONS ===")
	for i, rec := range recommendations {
		c.logger.Info("  %d: %s (%s)", i+1, rec.Name, rec.System)
	}
	c.logger.Info("=====================================")

	return recommendations
}

// ✅ NEW: Verify and improve system balance in recommendations
func (c *Client) verifySystemBalance(recommendations []GameRecommendation, targetSystems []string, targetCount int) []GameRecommendation {
	if len(targetSystems) <= 1 || len(recommendations) <= len(targetSystems) {
		return recommendations // No balance needed
	}

	// Count games per system
	systemCounts := make(map[string]int)
	systemGames := make(map[string][]GameRecommendation)

	for _, rec := range recommendations {
		systemCounts[rec.System]++
		systemGames[rec.System] = append(systemGames[rec.System], rec)
	}

	// Check if balance is good enough (no system should have more than 50% of total)
	maxAllowed := targetCount / 2
	if maxAllowed < 1 {
		maxAllowed = 1
	}

	needsRebalancing := false
	for _, count := range systemCounts {
		if count > maxAllowed {
			needsRebalancing = true
			break
		}
	}

	// Check if any selected system is completely missing
	for _, system := range targetSystems {
		if systemCounts[system] == 0 {
			needsRebalancing = true
			break
		}
	}

	if !needsRebalancing {
		return recommendations // Balance is acceptable
	}

	c.logger.Info("claude playlist: rebalancing recommendations across systems")

	// Create a more balanced selection
	targetPerSystem := targetCount / len(targetSystems)
	remainder := targetCount % len(targetSystems)

	var balanced []GameRecommendation
	usedGames := make(map[string]bool)

	// First pass: try to get targetPerSystem from each system
	for _, system := range targetSystems {
		count := 0
		target := targetPerSystem
		if remainder > 0 {
			target++
			remainder--
		}

		for _, game := range systemGames[system] {
			if count >= target {
				break
			}
			if !usedGames[game.Name] {
				balanced = append(balanced, game)
				usedGames[game.Name] = true
				count++
			}
		}
	}

	// Second pass: fill remaining slots from any system
	if len(balanced) < targetCount {
		for _, rec := range recommendations {
			if len(balanced) >= targetCount {
				break
			}
			if !usedGames[rec.Name] {
				balanced = append(balanced, rec)
				usedGames[rec.Name] = true
			}
		}
	}

	c.logger.Info("claude playlist: rebalanced %d games across %d systems", len(balanced), len(targetSystems))
	return balanced
}

// parseSuggestions extracts suggestions from Claude's response
func (c *Client) parseSuggestions(content string) []string {
	var parts []string

	if strings.Contains(content, "\n\n") {
		parts = strings.Split(strings.TrimSpace(content), "\n\n")
	} else {
		parts = strings.Split(strings.TrimSpace(content), "\n")
	}

	suggestions := make([]string, 0)

	for _, part := range parts {
		part = strings.TrimSpace(part)

		if part == "" {
			continue
		}

		// Remove common prefixes
		part = strings.TrimPrefix(part, "- ")
		part = strings.TrimPrefix(part, "• ")
		part = strings.TrimPrefix(part, "* ")
		part = strings.TrimPrefix(part, "1. ")
		part = strings.TrimPrefix(part, "2. ")
		part = strings.TrimPrefix(part, "3. ")

		part = strings.TrimSpace(part)

		if len(part) >= 20 && len(part) <= 300 {
			suggestions = append(suggestions, part)
		}

		if len(suggestions) >= 3 {
			break
		}
	}

	if len(suggestions) > 0 {
		c.logger.Info("claude: parsed %d suggestions successfully", len(suggestions))
		return suggestions
	}

	c.logger.Warn("claude: suggestion parsing failed, using fallback")
	return []string{
		"Try exploring different strategies",
		"Check for hidden mechanics or features",
		"Practice timing and precision",
	}
}

// extractArcadeGameName attempts to match a core name to an arcade game
func (c *Client) extractArcadeGameName(coreName string) string {
	c.logger.Info("claude debug: === EXTRACTING ARCADE NAME ===")
	c.logger.Info("claude debug: Looking for arcade name for core: '%s'", coreName)

	arcadeDir := "/media/fat/_Arcade"

	entries, err := os.ReadDir(arcadeDir)
	if err != nil {
		c.logger.Error("claude debug: ❌ Could not read _Arcade directory: %s", err)
		return ""
	}

	c.logger.Info("claude debug: Found %d entries in _Arcade directory", len(entries))

	coreNameLower := strings.ToLower(coreName)
	bestMatch := ""
	bestScore := 0
	totalMraFiles := 0

	// Debug: Show first few .mra files
	mraFiles := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filename := entry.Name()
		if !strings.HasSuffix(strings.ToLower(filename), ".mra") {
			continue
		}

		totalMraFiles++
		if len(mraFiles) < 10 { // Show first 10 for debugging
			mraFiles = append(mraFiles, filename)
		}

		nameWithoutExt := strings.TrimSuffix(filename, ".mra")
		nameWithoutExtLower := strings.ToLower(nameWithoutExt)

		baseName := nameWithoutExt
		if idx := strings.Index(nameWithoutExt, "("); idx != -1 {
			baseName = strings.TrimSpace(nameWithoutExt[:idx])
		}
		baseNameLower := strings.ToLower(baseName)

		score := c.calculateMatchScore(coreNameLower, baseNameLower, nameWithoutExtLower)

		if score > bestScore {
			bestScore = score
			bestMatch = baseName
			c.logger.Info("claude debug: New best match: '%s' -> '%s' (score: %d)", coreName, baseName, score)
		}
	}

	c.logger.Info("claude debug: Total MRA files: %d", totalMraFiles)
	c.logger.Info("claude debug: Sample MRA files: %v", mraFiles)
	c.logger.Info("claude debug: Best match: '%s' with score %d", bestMatch, bestScore)

	if bestScore >= 70 {
		c.logger.Info("claude debug: ✅ MATCH ACCEPTED: core '%s' -> arcade game '%s' (score: %d)", coreName, bestMatch, bestScore)
		return bestMatch
	}

	c.logger.Info("claude debug: ❌ MATCH REJECTED: score %d < 70, trying fallback", bestScore)

	if c.isLikelyArcadeCore(coreName) {
		cleaned := c.cleanCoreName(coreName)
		c.logger.Info("claude debug: ✅ FALLBACK: Using cleaned core name '%s' -> '%s'", coreName, cleaned)
		return cleaned
	}

	c.logger.Info("claude debug: ❌ NOT ARCADE: Core '%s' doesn't appear to be arcade", coreName)
	return ""
}

// calculateMatchScore computes similarity between core and game names
func (c *Client) calculateMatchScore(coreName, baseName, fullName string) int {
	score := 0

	if coreName == baseName {
		return 100
	}

	if strings.HasPrefix(coreName, baseName) {
		score += 80
	}

	if strings.HasPrefix(baseName, coreName) {
		score += 75
	}

	if strings.Contains(coreName, baseName) {
		score += 60
	}

	if strings.Contains(baseName, coreName) {
		score += 65
	}

	if len(coreName) == len(baseName)+1 && strings.HasPrefix(coreName, baseName) {
		score += 85
	}

	coreAbbrev := c.removeVowels(coreName)
	baseAbbrev := c.removeVowels(baseName)
	if coreAbbrev == baseAbbrev {
		score += 70
	}

	if c.similarStrings(coreName, baseName) {
		score += 50
	}

	return score
}

// removeVowels removes vowels from a string for fuzzy matching
func (c *Client) removeVowels(s string) string {
	vowels := "aeiou"
	result := ""
	for _, char := range strings.ToLower(s) {
		if !strings.ContainsRune(vowels, char) {
			result += string(char)
		}
	}
	return result
}

// similarStrings checks if two strings are similar enough
func (c *Client) similarStrings(a, b string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}

	lengthDiff := len(a) - len(b)
	if lengthDiff < 0 {
		lengthDiff = -lengthDiff
	}

	if lengthDiff > 2 {
		return false
	}

	commonChars := 0
	shorter := a
	if len(b) < len(a) {
		shorter = b
	}

	for i := 0; i < len(shorter); i++ {
		if i < len(a) && i < len(b) && a[i] == b[i] {
			commonChars++
		}
	}

	return float64(commonChars)/float64(len(shorter)) >= 0.7
}

// isLikelyArcadeCore determines if a core name likely represents an arcade game
func (c *Client) isLikelyArcadeCore(coreName string) bool {
	knownSystems := []string{
		// CONSOLES
		"AdventureVision", "Arcadia", "Astrocade", "Atari2600", "Atari5200", "Atari7800",
		"AtariLynx", "CasioPV1000", "CasioPV2000", "ChannelF", "ColecoVision", "CreatiVision",
		"FDS", "Gamate", "Gameboy", "Gameboy2P", "GameboyColor", "GameNWatch", "GBA", "GBA2P",
		"Genesis", "Intellivision", "Jaguar", "MasterSystem", "MegaDuck", "NES", "NeoGeo",
		"NeoGeoCD", "Nintendo64", "Odyssey2", "PCFX", "PokemonMini", "PSX", "Saturn", "Sega32X",
		"SegaCD", "SG1000", "SMS", "SNES", "SuperGameboy", "SuperGrafx", "SuperVision",
		"Tamagotchi", "TurboGrafx16", "TurboGrafx16CD", "VC4000", "Vectrex", "WonderSwan",
		"WonderSwanColor",

		// COMPUTERS
		"AcornAtom", "AcornElectron", "AliceMC10", "Amiga", "AmigaCD32", "Amstrad", "AmstradPCW",
		"Apogee", "AppleI", "AppleII", "Aquarius", "Atari800", "AtariST", "BBCMicro", "BK0011M",
		"C64", "ChipTest", "CoCo2", "CoCo3", "EDSAC", "Galaksija", "Interact", "Jupiter",
		"Laser", "Lynx48", "Macintosh", "MegaST", "MO5", "MSX", "MultiComp", "Orao", "Oric",
		"PC88", "PDP1", "PET2001", "PMD85", "RX78", "SAMCoupe", "SharpMZ", "SordM5",
		"Specialist", "TI994A", "TRS80", "TSConf", "UK101", "Vector06", "VIC20", "X68000",
		"ZX81", "ZXSpectrum",

		// OTHER SYSTEMS
		"Arduboy", "Chip8", "FlappyBird", "Groovy",

		// COMMON ALIASES
		"TGFX16", "PCE", "GG", "GameGear", "N64", "A7800", "LYNX", "NGP", "WS",
	}

	for _, system := range knownSystems {
		if strings.EqualFold(coreName, system) {
			return false
		}
	}

	// Typical arcade patterns
	arcadePatterns := []string{
		"194", "195", "196", "197", "198", "199",
		"pac", "kong", "man", "fighter", "force", "strike",
	}

	coreNameLower := strings.ToLower(coreName)
	for _, pattern := range arcadePatterns {
		if strings.Contains(coreNameLower, pattern) {
			return true
		}
	}

	// Only if the name is very short AND not in known systems
	return len(coreName) <= 8
}

// cleanCoreName cleans up a core name for display
func (c *Client) cleanCoreName(coreName string) string {
	name := strings.TrimSuffix(coreName, ".rbf")
	name = strings.TrimSuffix(name, "_MiSTer")

	words := strings.Fields(name)
	for i, word := range words {
		if len(word) > 0 {
			words[i] = strings.Title(strings.ToLower(word))
		}
	}

	return strings.Join(words, " ")
}

// callAnthropicAPI makes the actual HTTP request to Claude API
func (c *Client) callAnthropicAPI(ctx context.Context, request *AnthropicRequest) (*AnthropicResponse, error) {
	jsonData, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", anthropicAPIURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.config.APIKey)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api returned status %d", resp.StatusCode)
	}

	var apiResponse AnthropicResponse
	err = json.NewDecoder(resp.Body).Decode(&apiResponse)
	if err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &apiResponse, nil
}

// getOrCreateSession gets or creates a chat session
func (c *Client) getOrCreateSession(sessionID string) *ChatSession {
	c.sessionMux.Lock()
	defer c.sessionMux.Unlock()

	if sessionID == "" {
		sessionID = fmt.Sprintf("session_%d", time.Now().Unix())
	}

	session, exists := c.sessions[sessionID]
	if !exists {
		session = &ChatSession{
			ID:       sessionID,
			Messages: make([]Message, 0),
			Created:  time.Now(),
			Updated:  time.Now(),
		}
		c.sessions[sessionID] = session
	}

	return session
}

// trimSessionHistory keeps session history within configured limits
func (c *Client) trimSessionHistory(session *ChatSession) {
	if len(session.Messages) > c.config.ChatHistory*2 {
		start := len(session.Messages) - c.config.ChatHistory*2
		session.Messages = session.Messages[start:]
	}
}

// =========================================
// ADD THESE NEW FUNCTIONS TO client.go
// Do NOT replace existing functions
// =========================================

// GeneratePlaylistFromActiveGame creates a playlist based on the currently active game
func (c *Client) GeneratePlaylistFromActiveGame(ctx context.Context, request *PlaylistRequest, trk *tracker.Tracker) (*PlaylistResponse, error) {
	if !c.config.Enabled {
		return &PlaylistResponse{
			Error:     "Claude is disabled",
			Timestamp: time.Now(),
		}, nil
	}

	// Get current game context
	gameContext := c.BuildGameContext(trk)
	if gameContext.GameName == "" {
		return &PlaylistResponse{
			Error:     "No game currently active to base playlist on",
			Theme:     request.Theme,
			Timestamp: time.Now(),
		}, nil
	}

	// Build specialized prompt for active game
	prompt := c.buildActiveGamePlaylistPrompt(request, gameContext)

	response, err := c.SendMessage(ctx, prompt, gameContext, "playlist")
	if err != nil {
		return &PlaylistResponse{
			Error:     "Failed to generate playlist based on active game",
			Theme:     request.Theme,
			Timestamp: time.Now(),
		}, nil
	}

	if response.Error != "" {
		return &PlaylistResponse{
			Error:     response.Error,
			Theme:     request.Theme,
			Timestamp: time.Now(),
		}, nil
	}

	// Parse and validate game recommendations
	games := c.parseGameRecommendations(response.Content, request.GameCount, request.InstalledGames)

	// Set theme to reflect active game context
	finalTheme := request.Theme
	if finalTheme == "" {
		finalTheme = fmt.Sprintf("Games similar to %s", gameContext.GameName)
	}

	return &PlaylistResponse{
		Games:     games,
		Theme:     finalTheme,
		Timestamp: time.Now(),
	}, nil
}

// buildActiveGamePlaylistPrompt creates a specialized prompt for active game-based playlists
func (c *Client) buildActiveGamePlaylistPrompt(request *PlaylistRequest, gameContext *GameContext) string {
	if len(request.InstalledGames) == 0 {
		// Fallback to generic recommendations if no games provided
		return c.buildActiveGamePlaylistPromptGeneric(request, gameContext)
	}

	// Build list of user's actual games for Claude
	var gamesList strings.Builder
	gamesList.WriteString("AVAILABLE GAMES IN USER'S COLLECTION:\n\n")

	// Group games by system for better organization
	systemGames := make(map[string][]InstalledGame)
	for _, game := range request.InstalledGames {
		systemGames[game.System] = append(systemGames[game.System], game)
	}

	for system, games := range systemGames {
		gamesList.WriteString(fmt.Sprintf("%s (%d games available):\n", system, len(games)))

		// ✅ SAME FIX: Apply the same randomization as in buildPlaylistPrompt
		orderedGames := make([]InstalledGame, len(games))
		copy(orderedGames, games)

		// Reverse the list to start with Z instead of A
		for i, j := 0, len(orderedGames)-1; i < j; i, j = i+1, j-1 {
			orderedGames[i], orderedGames[j] = orderedGames[j], orderedGames[i]
		}

		// Add time-based rotation to vary the starting point
		offset := int(time.Now().Second()) % len(orderedGames)
		rotatedGames := make([]InstalledGame, len(orderedGames))
		for i, game := range orderedGames {
			newIndex := (i + offset) % len(orderedGames)
			rotatedGames[newIndex] = game
		}

		for _, game := range rotatedGames {
			gamesList.WriteString(fmt.Sprintf("- %s\n", game.Name))
		}
		gamesList.WriteString("\n")
	}

	// Determine the theme context
	themeContext := ""
	if request.Theme != "" && !isActiveGameThemeKeyword(request.Theme) {
		themeContext = fmt.Sprintf(" with focus on: %s", request.Theme)
	}

	prompt := fmt.Sprintf(`You are generating a curated game playlist for a MiSTer FPGA user based on their currently active game.

CURRENTLY PLAYING: %s (%s system)
REQUESTED COUNT: %d games
SELECTED SYSTEMS: %v

%s

CORE INSTRUCTION:
Generate a playlist of games similar to "%s" that the user is currently playing%s.

SIMILARITY CRITERIA:
- Genre and gameplay style
- Art style and visual presentation  
- Difficulty level and game mechanics
- Time period or setting (if relevant)
- Overall "feel" and atmosphere

CRITICAL REQUIREMENTS:
- You MUST only recommend games from the user's collection listed above
- Do NOT recommend games that are not in the list
- Do NOT include the currently active game ("%s") in the recommendations
- Focus on games that share DNA with the active game
- If multiple systems are selected, provide variety across systems when possible
- Prioritize quality matches over quantity

RANDOMIZATION REQUIREMENT:
- Do NOT list games in alphabetical order
- ACTIVELY AVOID alphabetical patterns (A, B, C games)  
- Mix different starting letters randomly in your recommendations
- Prioritize similarity to "%s" over alphabetical ordering
- If you notice alphabetical bias, deliberately break it by choosing games that start with different letters
- Ensure variety in the first letters of recommended games (avoid clustering around A-D)

FORMAT REQUIREMENTS:
Format each recommendation exactly as:
Game Name | System | Brief description | Why it's similar to %s

Only recommend games that appear exactly in the user's collection above.`,
		gameContext.GameName,
		gameContext.SystemName,
		request.GameCount,
		request.Systems,
		gamesList.String(),
		gameContext.GameName,
		themeContext,
		gameContext.GameName,
		gameContext.GameName,
		gameContext.GameName)

	return prompt
}

// buildActiveGamePlaylistPromptGeneric provides fallback for when no games are provided
func (c *Client) buildActiveGamePlaylistPromptGeneric(request *PlaylistRequest, gameContext *GameContext) string {
	themeContext := ""
	if request.Theme != "" && !isActiveGameThemeKeyword(request.Theme) {
		themeContext = fmt.Sprintf(" with focus on: %s", request.Theme)
	}

	prompt := fmt.Sprintf(`Generate exactly %d game recommendations similar to "%s" (%s system)%s.

Look for games that share:
- Similar gameplay mechanics
- Comparable visual style
- Similar difficulty or complexity
- Related themes or settings

RANDOMIZATION REQUIREMENT:
- Do NOT list games in alphabetical order
- ACTIVELY AVOID alphabetical patterns (A, B, C games)
- Mix different starting letters randomly in your recommendations
- Prioritize similarity to "%s" over alphabetical ordering
- Ensure variety in the first letters of recommended games (avoid clustering around A-D)

`,
		request.GameCount, gameContext.GameName, gameContext.SystemName, themeContext, gameContext.GameName)

	if len(request.Systems) > 0 {
		prompt += fmt.Sprintf("Focus on these systems: %v\n", request.Systems)
	}

	if request.Preferences != "" {
		prompt += fmt.Sprintf("User preferences: %s\n", request.Preferences)
	}

	prompt += "Format each game as: Game Name | System | Brief description | Why it's similar"
	return prompt
}

// GetActiveGameSuggestion returns a dynamic suggestion based on the current active game
func (c *Client) GetActiveGameSuggestion(trk *tracker.Tracker) string {
	gameContext := c.BuildGameContext(trk)
	if gameContext.GameName == "" {
		return "Similar games to active game" // fallback when no game is active
	}

	return fmt.Sprintf("Games similar to %s", gameContext.GameName)
}

func (c *Client) buildPlaylistPrompt(request *PlaylistRequest) string {
	if len(request.InstalledGames) == 0 {
		// Fallback to generic recommendations if no games provided
		return c.buildPlaylistPromptGeneric(request)
	}

	// ✅ QUICK DEBUG: Log the raw games we're sending to Claude
	c.logger.Info("=== CLAUDE INPUT DEBUG ===")
	c.logger.Info("Theme: %s", request.Theme)
	c.logger.Info("Games being sent to Claude (first 15):")
	for i := 0; i < 15 && i < len(request.InstalledGames); i++ {
		game := request.InstalledGames[i]
		c.logger.Info("  %d: %s (%s)", i+1, game.Name, game.System)
	}
	c.logger.Info("==========================")

	// Build list of user's actual games for Claude
	var gamesList strings.Builder
	gamesList.WriteString("AVAILABLE GAMES IN USER'S COLLECTION:\n\n")

	// Group games by system for better organization and balance analysis
	systemGames := make(map[string][]InstalledGame)
	for _, game := range request.InstalledGames {
		systemGames[game.System] = append(systemGames[game.System], game)
	}

	// ✅ NEW: Calculate target games per system for balanced distribution
	targetPerSystem := ""
	if len(request.Systems) > 1 && request.GameCount > len(request.Systems) {
		minPerSystem := request.GameCount / len(request.Systems)
		remainder := request.GameCount % len(request.Systems)

		if minPerSystem > 0 {
			targetPerSystem = fmt.Sprintf("\nTARGET DISTRIBUTION: Aim for %d-%d games per system to ensure balanced variety.",
				minPerSystem, minPerSystem+1)
			if remainder > 0 {
				targetPerSystem += fmt.Sprintf(" (%d systems can have +1 extra game)", remainder)
			}
		}
	}

	// ✅ SIMPLE FIX: Reverse order + time-based rotation
	for system, games := range systemGames {
		gamesList.WriteString(fmt.Sprintf("%s (%d games available):\n", system, len(games)))

		// ✅ SIMPLE FIX: Reverse order + time-based rotation
		orderedGames := make([]InstalledGame, len(games))
		copy(orderedGames, games)

		// Reverse the list to start with Z instead of A
		for i, j := 0, len(orderedGames)-1; i < j; i, j = i+1, j-1 {
			orderedGames[i], orderedGames[j] = orderedGames[j], orderedGames[i]
		}

		// Add time-based rotation to vary the starting point
		offset := int(time.Now().Second()) % len(orderedGames)
		rotatedGames := make([]InstalledGame, len(orderedGames))
		for i, game := range orderedGames {
			newIndex := (i + offset) % len(orderedGames)
			rotatedGames[newIndex] = game
		}

		for _, game := range rotatedGames {
			gamesList.WriteString(fmt.Sprintf("- %s\n", game.Name))
		}
		gamesList.WriteString("\n")
	}

	prompt := fmt.Sprintf(`You are generating a curated game playlist for a MiSTer FPGA user.

THEME: %s
REQUESTED COUNT: %d games
SELECTED SYSTEMS: %v%s

%s

CRITICAL INSTRUCTIONS:
- You MUST only recommend games from the user's collection listed above
- Do NOT recommend games that are not in the list
- Select games that best match the theme "%s"
- BALANCE REQUIREMENT: When multiple systems are selected, provide good variety across ALL selected systems
- Avoid recommending all games from just one or two systems
- Focus on quality and theme relevance while maintaining system balance
- If a system has fewer games available, that's acceptable, but try to include at least 1-2 games from each system when possible

EXTREMELY IMPORTANT - ALPHABETICAL DIVERSITY REQUIREMENT:
- Do NOT list games in alphabetical order under ANY circumstances
- ACTIVELY AVOID recommending multiple games that start with the same letter
- Mix different starting letters randomly in your recommendations
- Prioritize theme relevance over alphabetical ordering
- If you notice your recommendations start with similar letters (like A, B, C), STOP and choose games that start with different letters instead
- Ensure variety in the first letters of recommended games (avoid clustering around A-D)
- This is CRITICAL - users have complained about alphabetical bias in recommendations

FORMAT REQUIREMENTS:
Format each recommendation exactly as:
Game Name | System | Brief description | Why it fits the theme

Only recommend games that appear exactly in the user's collection above.`,
		request.Theme,
		request.GameCount,
		request.Systems,
		targetPerSystem,
		gamesList.String(),
		request.Theme)

	// ✅ QUICK DEBUG: Log the actual prompt sent to Claude
	c.logger.Info("=== PROMPT SENT TO CLAUDE ===")
	c.logger.Info("Full prompt length: %d characters", len(prompt))
	c.logger.Info("Prompt preview (first 500 chars): %s", prompt[:min(len(prompt), 500)])
	c.logger.Info("==============================")

	return prompt
}

// Helper function for min (add at the end of the file if it doesn't exist)
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
