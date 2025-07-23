package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

	if c.config.APIKey == "" {
		return &ChatResponse{
			Error:     "Claude API key not configured",
			Timestamp: time.Now(),
		}, nil
	}

	// Get or create session
	session := c.getOrCreateSession(sessionID)

	// Build context-aware prompt
	prompt := c.buildContextualPrompt(message, gameContext)

	// Add user message to session
	userMsg := Message{
		Role:    "user",
		Content: prompt,
	}
	session.Messages = append(session.Messages, userMsg)

	// Trim session history if too long
	c.trimSessionHistory(session)

	// Prepare API request
	reqBody := AnthropicRequest{
		Model:     c.config.Model,
		MaxTokens: maxTokens,
		Messages:  session.Messages,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", anthropicAPIURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.config.APIKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("User-Agent", userAgent)

	// Send request
	c.logger.Info("claude: sending request to API")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	// Parse response
	var apiResp AnthropicResponse
	err = json.NewDecoder(resp.Body).Decode(&apiResp)
	if err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Check for API errors
	if apiResp.Error.Message != "" {
		c.logger.Error("claude API error: %s", apiResp.Error.Message)
		return &ChatResponse{
			Error:     fmt.Sprintf("API Error: %s", apiResp.Error.Message),
			Timestamp: time.Now(),
		}, nil
	}

	// Extract content
	if len(apiResp.Content) == 0 {
		return &ChatResponse{
			Error:     "No content in API response",
			Timestamp: time.Now(),
		}, nil
	}

	content := apiResp.Content[0].Text
	if content == "" {
		return &ChatResponse{
			Error:     "Empty response from Claude",
			Timestamp: time.Now(),
		}, nil
	}

	// Add assistant response to session
	assistantMsg := Message{
		Role:    "assistant",
		Content: content,
	}
	session.Messages = append(session.Messages, assistantMsg)

	c.logger.Info("claude: response received successfully")

	return &ChatResponse{
		Content:   content,
		Timestamp: time.Now(),
		Context:   gameContext,
	}, nil
}

// GenerateSuggestions creates contextual suggestions for the current game
func (c *Client) GenerateSuggestions(ctx context.Context, gameContext *GameContext) (*SuggestionsResponse, error) {
	if !c.config.Enabled {
		return &SuggestionsResponse{
			Error:     "Claude is disabled",
			Timestamp: time.Now(),
		}, nil
	}

	prompt := c.buildSuggestionsPrompt(gameContext)

	response, err := c.SendMessage(ctx, prompt, gameContext, "suggestions")
	if err != nil {
		return &SuggestionsResponse{
			Error:     "Failed to generate suggestions",
			Timestamp: time.Now(),
		}, nil
	}

	if response.Error != "" {
		return &SuggestionsResponse{
			Error:     response.Error,
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

// buildContextualPrompt creates a context-aware prompt for Claude
func (c *Client) buildContextualPrompt(message string, gameContext *GameContext) string {
	if gameContext == nil || gameContext.GameName == "" {
		return message
	}

	if gameContext.SystemName == "Arcade" || strings.Contains(strings.ToLower(gameContext.SystemName), "arcade") {
		return fmt.Sprintf(`Current game context: %s (%s) - This is an arcade game.

User question: %s

Please provide helpful advice about this specific arcade game. Focus on gameplay tips, strategies, or interesting facts about this game.`,
			gameContext.GameName, gameContext.SystemName, message)
	}

	return fmt.Sprintf(`Current game context: %s on %s

User question: %s

Please provide helpful advice about this specific game. Focus on gameplay tips, strategies, or interesting facts.`,
		gameContext.GameName, gameContext.SystemName, message)
}

// buildSuggestionsPrompt creates a prompt for generating game suggestions
func (c *Client) buildSuggestionsPrompt(gameContext *GameContext) string {
	if gameContext == nil || gameContext.GameName == "" {
		return "Generate 3 brief gaming tips for retro game enthusiasts."
	}

	return fmt.Sprintf(`Current game: %s (%s)

Generate exactly 3 brief, helpful suggestions for playing this specific game. Each suggestion should be:
- One clear sentence
- Actionable and specific to this game
- About gameplay strategy, tips, or interesting mechanics

Format as a simple numbered list:
1. [suggestion]
2. [suggestion]  
3. [suggestion]`,
		gameContext.GameName, gameContext.SystemName)
}

// parseSuggestions extracts suggestions from Claude's response
func (c *Client) parseSuggestions(content string) []string {
	lines := strings.Split(content, "\n")
	var suggestions []string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Remove list numbering
		part := strings.TrimPrefix(line, "1. ")
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

// parseGameRecommendations extracts game names from Claude's playlist response
func (c *Client) parseGameRecommendations(content string, maxGames int, installedGames []InstalledGame) []GameRecommendation {
	lines := strings.Split(content, "\n")
	var games []GameRecommendation
	installed := make(map[string]bool)

	// Create lookup for installed games
	for _, game := range installedGames {
		installed[strings.ToLower(game.Name)] = true
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Remove list markers
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		for i := 1; i <= 20; i++ {
			line = strings.TrimPrefix(line, fmt.Sprintf("%d. ", i))
		}

		line = strings.TrimSpace(line)

		if len(line) >= 3 && len(line) <= 100 {
			games = append(games, GameRecommendation{
				Name:        line,
				System:      "Unknown",
				Path:        "",
				Description: "",
				Reason:      "Recommended by Claude AI",
				GeneratedAt: time.Now(),
				Theme:       "",
			})
		}

		if len(games) >= maxGames {
			break
		}
	}

	return games
}

// getOrCreateSession retrieves or creates a chat session
func (c *Client) getOrCreateSession(sessionID string) *ChatSession {
	c.sessionMux.Lock()
	defer c.sessionMux.Unlock()

	if sessionID == "" {
		sessionID = "default"
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

// BuildGameContext extracts current game information from tracker
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

	// ✅ PRIORITY 1: Check if SAM is active (with improved detection)
	if samInfo := c.checkSAMStatus(); samInfo != nil {
		c.logger.Info("claude debug: SAM detected, using SAM game info")
		context.GameName = samInfo.GameName
		context.CoreName = samInfo.CoreName
		context.SystemName = samInfo.SystemName
		return context
	}

	// ✅ PRIORITY 2: Fix arcade detection when SAM is NOT active
	if context.CoreName != "" {
		c.logger.Info("claude debug: SAM not active, checking arcade detection for core '%s'", context.CoreName)

		// ✅ IMPROVED: Always check if it's arcade, regardless of what tracker says
		if arcadeName := c.extractArcadeGameName(context.CoreName); arcadeName != "" {
			c.logger.Info("claude debug: ✅ ARCADE DETECTED: Core '%s' -> Game '%s'", context.CoreName, arcadeName)
			context.GameName = arcadeName
			context.SystemName = "Arcade"
			context.GamePath = ""
			return context
		}

		// ✅ IMPROVED: Fallback for known arcade cores without MRA match
		if c.isLikelyArcadeCore(context.CoreName) {
			c.logger.Info("claude debug: ✅ ARCADE FALLBACK: Core '%s' treated as arcade", context.CoreName)
			context.GameName = context.CoreName
			context.SystemName = "Arcade"
			context.GamePath = ""
			return context
		}

		c.logger.Info("claude debug: ✅ NON-ARCADE: Core '%s' is not arcade, keeping tracker data", context.CoreName)
	}

	c.logger.Info("claude debug: === FINAL CONTEXT ===")
	c.logger.Info("claude debug: Final GameName = '%s'", context.GameName)
	c.logger.Info("claude debug: Final SystemName = '%s'", context.SystemName)
	c.logger.Info("claude debug: Final CoreName = '%s'", context.CoreName)

	return context
}

// ✅ MODIFIED: Main function to use samindex first
func (c *Client) extractArcadeGameName(coreName string) string {
	c.logger.Info("claude debug: === EXTRACTING ARCADE NAME ===")
	c.logger.Info("claude debug: Looking for arcade name for core: '%s'", coreName)

	// 🎯 PRIORITY 1: Use samindex directly (like SAM does)
	if name := c.extractArcadeGameNameUsingSamindex(coreName); name != "" {
		c.logger.Info("claude debug: ✅ SUCCESS via samindex: '%s' -> '%s'", coreName, name)
		return name
	}

	// 🎯 PRIORITY 2: Fallback to previous method if samindex fails
	c.logger.Info("claude debug: ⚠️ samindex failed, trying fallback method")
	if name := c.extractArcadeGameNameFallback(coreName); name != "" {
		c.logger.Info("claude debug: ✅ SUCCESS via fallback: '%s' -> '%s'", coreName, name)
		return name
	}

	// 🎯 PRIORITY 3: Last resort - cleaned core name
	if c.isLikelyArcadeCore(coreName) {
		cleaned := c.cleanCoreName(coreName)
		c.logger.Info("claude debug: ✅ LAST RESORT: Using cleaned core name '%s' -> '%s'", coreName, cleaned)
		return cleaned
	}

	c.logger.Info("claude debug: ❌ COMPLETE FAILURE: No arcade name found for core '%s'", coreName)
	return ""
}

// ✅ NEW IMPLEMENTATION: Use samindex directly like SAM does
func (c *Client) extractArcadeGameNameUsingSamindex(coreName string) string {
	c.logger.Info("claude debug: === USING SAMINDEX DIRECTLY ===")
	c.logger.Info("claude debug: Looking for arcade game for core: '%s'", coreName)

	// 1. Define paths like SAM does
	mrsampath := "/media/fat/Scripts/.MiSTer_SAM"
	samindexBinary := filepath.Join(mrsampath, "samindex")
	tempGamelistPath := "/tmp"
	gamelistFile := filepath.Join(tempGamelistPath, "arcade_gamelist.txt")

	// 2. Verify samindex exists
	if _, err := os.Stat(samindexBinary); err != nil {
		c.logger.Error("claude debug: ❌ samindex not found at %s: %s", samindexBinary, err)
		return ""
	}

	// 3. Execute samindex exactly like SAM: samindex -s arcade -o /tmp
	c.logger.Info("claude debug: 🔧 Executing: %s -q -s arcade -o %s", samindexBinary, tempGamelistPath)
	cmd := exec.Command(samindexBinary, "-q", "-s", "arcade", "-o", tempGamelistPath)

	// Execute the command
	output, err := cmd.CombinedOutput()
	if err != nil {
		c.logger.Error("claude debug: ❌ samindex execution failed: %s, output: %s", err, string(output))
		return ""
	}

	c.logger.Info("claude debug: ✅ samindex executed successfully")

	// 4. Read the generated gamelist file
	if _, err := os.Stat(gamelistFile); err != nil {
		c.logger.Error("claude debug: ❌ Gamelist file not found: %s", gamelistFile)
		return ""
	}

	content, err := os.ReadFile(gamelistFile)
	if err != nil {
		c.logger.Error("claude debug: ❌ Could not read gamelist file: %s", err)
		return ""
	}

	c.logger.Info("claude debug: 📄 Reading gamelist with %d bytes", len(content))

	// 5. Search for the core in MRA file paths
	lines := strings.Split(string(content), "\n")
	coreNameLower := strings.ToLower(coreName)

	c.logger.Info("claude debug: 🔍 Searching for core '%s' in %d MRA files", coreName, len(lines))

	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Log first 5 lines for debugging
		if i < 5 {
			c.logger.Info("claude debug: Sample MRA line %d: %s", i+1, line)
		}

		// Extract filename without extension
		filename := filepath.Base(line)
		nameWithoutExt := strings.TrimSuffix(filename, ".mra")
		nameWithoutExtLower := strings.ToLower(nameWithoutExt)

		// Look for match with core name
		if strings.Contains(nameWithoutExtLower, coreNameLower) {
			c.logger.Info("claude debug: 🎯 MATCH FOUND: '%s' matches '%s'", coreName, nameWithoutExt)

			// Try to extract full name from MRA file
			if fullName := c.extractGameNameFromMRA(line); fullName != "" {
				c.logger.Info("claude debug: ✅ SUCCESS: Extracted full name from MRA: '%s'", fullName)
				return fullName
			}

			// Fallback to filename
			c.logger.Info("claude debug: ✅ SUCCESS: Using filename: '%s'", nameWithoutExt)
			return nameWithoutExt
		}
	}

	c.logger.Info("claude debug: ❌ No match found for core '%s' in arcade gamelist", coreName)
	return ""
}

// ✅ RENAMED: Previous method as fallback
func (c *Client) extractArcadeGameNameFallback(coreName string) string {
	c.logger.Info("claude debug: === FALLBACK METHOD ===")

	arcadeDir := "/media/fat/_Arcade"

	entries, err := os.ReadDir(arcadeDir)
	if err != nil {
		c.logger.Error("claude debug: ❌ Could not read _Arcade directory: %s", err)
		return ""
	}

	c.logger.Info("claude debug: Found %d entries in _Arcade directory", len(entries))

	coreNameLower := strings.ToLower(coreName)
	bestMatch := ""
	bestMatchPath := ""
	bestScore := 0
	totalMraFiles := 0

	// Find best match by filename
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filename := entry.Name()
		if !strings.HasSuffix(strings.ToLower(filename), ".mra") {
			continue
		}

		totalMraFiles++

		nameWithoutExt := strings.TrimSuffix(filename, ".mra")
		nameWithoutExtLower := strings.ToLower(nameWithoutExt)

		// Extract base name (before first parenthesis)
		baseName := nameWithoutExt
		if idx := strings.Index(nameWithoutExt, "("); idx != -1 {
			baseName = strings.TrimSpace(nameWithoutExt[:idx])
		}
		baseNameLower := strings.ToLower(baseName)

		// Calculate score
		score := c.calculateMatchScore(coreNameLower, baseNameLower, nameWithoutExtLower)

		if score > bestScore {
			bestScore = score
			bestMatch = baseName
			bestMatchPath = filepath.Join(arcadeDir, filename)
		}
	}

	c.logger.Info("claude debug: Processed %d .mra files, best score: %d", totalMraFiles, bestScore)

	if bestScore == 0 {
		return ""
	}

	// Try to extract from XML if we have a match
	if bestMatchPath != "" {
		if xmlName := c.extractGameNameFromMRA(bestMatchPath); xmlName != "" {
			return xmlName
		}
	}

	// Minimum score to accept
	if bestScore >= 50 {
		return bestMatch
	}

	return ""
}

// ✅ NEW: Function to clean core names
func (c *Client) cleanCoreName(coreName string) string {
	// Remove common numbers and special characters
	cleaned := coreName
	cleaned = strings.TrimSuffix(cleaned, "h") // ssf2h -> ssf2
	cleaned = strings.TrimSuffix(cleaned, "t") // ssf2t -> ssf2

	// Convert known abbreviations
	replacements := map[string]string{
		"ssf2": "Super Street Fighter II",
		"sf2":  "Street Fighter II",
		"mk":   "Mortal Kombat",
		"kof":  "King of Fighters",
		"ms":   "Metal Slug",
	}

	if replacement, exists := replacements[strings.ToLower(cleaned)]; exists {
		return replacement
	}

	return cleaned
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

	return float64(commonChars)/float64(len(shorter)) > 0.6
}

// isLikelyArcadeCore determines if a core name suggests an arcade game
func (c *Client) isLikelyArcadeCore(coreName string) bool {
	if coreName == "" {
		return false
	}

	// Known non-arcade systems
	nonArcadeSystems := []string{
		"nes", "snes", "genesis", "megadrive", "n64", "psx", "saturn",
		"gb", "gbc", "gba", "gg", "sms", "tgfx16", "neogeo",
		"atari", "c64", "amiga", "ao486", "minimig",
	}

	coreNameLower := strings.ToLower(coreName)
	for _, system := range nonArcadeSystems {
		if strings.Contains(coreNameLower, system) {
			return false
		}
	}

	// If it's a short name without clear system indicators, likely arcade
	return len(coreName) <= 8 && !strings.Contains(coreName, "/")
}

// ✅ IMPROVED: Simple SAM detection with age check only
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

	// ✅ Check if SAM file is too old (obsolete)
	stat, err := os.Stat(samFile)
	if err != nil {
		c.logger.Info("claude debug: Cannot stat SAM file: %s", err)
		return nil
	}

	age := time.Since(stat.ModTime())
	maxAge := 3 * time.Minute // SAM file older than 3 minutes is considered obsolete

	c.logger.Info("claude debug: SAM file age: %v (max allowed: %v)", age, maxAge)

	if age > maxAge {
		c.logger.Info("claude debug: SAM file is too old (%v) - treating as obsolete", age)
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
		"neogeo":    "SNK NeoGeo",
		"psx":       "Sony PlayStation",
		"saturn":    "Sega Saturn",
		"n64":       "Nintendo 64",
		"gb":        "Game Boy",
		"gbc":       "Game Boy Color",
		"gba":       "Game Boy Advance",
	}

	if system, exists := systemMap[strings.ToLower(coreName)]; exists {
		return system
	}

	// Default to titlecase of core name
	return strings.Title(coreName)
}

// ✅ FINAL FIX: Extract game name from MRA XML with correct patterns
func (c *Client) extractGameNameFromMRA(mraPath string) string {
	content, err := os.ReadFile(mraPath)
	if err != nil {
		c.logger.Error("claude debug: ❌ Could not read MRA file %s: %s", mraPath, err)
		return ""
	}

	contentStr := string(content)

	// Show first 200 chars of MRA content for debugging
	preview := contentStr
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}
	c.logger.Info("claude debug: MRA content preview: %s", preview)

	// ✅ CORRECTED: Try all possible XML name patterns
	// Pattern 1: <name>Game Name</name> (full name tag)
	nameStart := strings.Index(contentStr, "<name>")
	if nameStart != -1 {
		nameStart += 6 // len("<name>")
		nameEnd := strings.Index(contentStr[nameStart:], "</name>")
		if nameEnd != -1 {
			gameName := strings.TrimSpace(contentStr[nameStart : nameStart+nameEnd])
			if gameName != "" {
				c.logger.Info("claude debug: ✅ Extracted from <name> tag: '%s'", gameName)
				return gameName
			}
		}
	}

	// Pattern 2: <n>Game Name</n> (short name tag)
	nameStart = strings.Index(contentStr, "<n>")
	if nameStart != -1 {
		nameStart += 3 // len("<n>")
		nameEnd := strings.Index(contentStr[nameStart:], "</n>")
		if nameEnd != -1 {
			gameName := strings.TrimSpace(contentStr[nameStart : nameStart+nameEnd])
			if gameName != "" {
				c.logger.Info("claude debug: ✅ Extracted from <n> tag: '%s'", gameName)
				return gameName
			}
		}
	}

	// Pattern 3: <setname>Game Name</setname>
	nameStart = strings.Index(contentStr, "<setname>")
	if nameStart != -1 {
		nameStart += 9 // len("<setname>")
		nameEnd := strings.Index(contentStr[nameStart:], "</setname>")
		if nameEnd != -1 {
			gameName := strings.TrimSpace(contentStr[nameStart : nameStart+nameEnd])
			if gameName != "" {
				c.logger.Info("claude debug: ✅ Extracted from <setname> tag: '%s'", gameName)
				return gameName
			}
		}
	}

	c.logger.Info("claude debug: ❌ No name tags found in MRA XML (tried: name, n, setname)")
	return ""
}

// Helper function for min (add at the end of the file if it doesn't exist)
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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

	if c.config.APIKey == "" {
		return &ChatResponse{
			Error:     "Claude API key not configured",
			Timestamp: time.Now(),
		}, nil
	}

	// Get or create session
	session := c.getOrCreateSession(sessionID)

	// Build context-aware prompt
	prompt := c.buildContextualPrompt(message, gameContext)

	// Add user message to session
	userMsg := AnthropicMessage{
		Role:    "user",
		Content: prompt,
	}
	session.Messages = append(session.Messages, userMsg)

	// Trim session history if too long
	c.trimSessionHistory(session)

	// Prepare API request
	reqBody := AnthropicRequest{
		Model:     c.config.Model,
		MaxTokens: maxTokens,
		Messages:  session.Messages,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, "POST", anthropicAPIURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.config.APIKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("User-Agent", userAgent)

	// Send request
	c.logger.Info("claude: sending request to API")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	// Parse response
	var apiResp AnthropicResponse
	err = json.NewDecoder(resp.Body).Decode(&apiResp)
	if err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	// Check for API errors
	if apiResp.Error != nil {
		c.logger.Error("claude API error: %s", apiResp.Error.Message)
		return &ChatResponse{
			Error:     fmt.Sprintf("API Error: %s", apiResp.Error.Message),
			Timestamp: time.Now(),
		}, nil
	}

	// Extract content
	if len(apiResp.Content) == 0 {
		return &ChatResponse{
			Error:     "No content in API response",
			Timestamp: time.Now(),
		}, nil
	}

	content := apiResp.Content[0].Text
	if content == "" {
		return &ChatResponse{
			Error:     "Empty response from Claude",
			Timestamp: time.Now(),
		}, nil
	}

	// Add assistant response to session
	assistantMsg := AnthropicMessage{
		Role:    "assistant",
		Content: content,
	}
	session.Messages = append(session.Messages, assistantMsg)

	c.logger.Info("claude: response received successfully")

	return &ChatResponse{
		Content:   content,
		Timestamp: time.Now(),
		Context:   gameContext,
	}, nil
}

// GenerateSuggestions creates contextual suggestions for the current game
func (c *Client) GenerateSuggestions(ctx context.Context, gameContext *GameContext) (*SuggestionsResponse, error) {
	if !c.config.Enabled {
		return &SuggestionsResponse{
			Error:     "Claude is disabled",
			Timestamp: time.Now(),
		}, nil
	}

	prompt := c.buildSuggestionsPrompt(gameContext)

	response, err := c.SendMessage(ctx, prompt, gameContext, "suggestions")
	if err != nil {
		return &SuggestionsResponse{
			Error:     "Failed to generate suggestions",
			Timestamp: time.Now(),
		}, nil
	}

	if response.Error != "" {
		return &SuggestionsResponse{
			Error:     response.Error,
			Timestamp: time.Now(),
		}, nil
	}

	suggestions := c.parseSuggestions(response.Content)

	return &SuggestionsResponse{
		Suggestions: suggestions,
		GameInfo:    gameContext,
		Generated:   time.Now(),
	}, nil
}

// buildContextualPrompt creates a context-aware prompt for Claude
func (c *Client) buildContextualPrompt(message string, gameContext *GameContext) string {
	if gameContext == nil || gameContext.GameName == "" {
		return message
	}

	if gameContext.SystemName == "Arcade" || strings.Contains(strings.ToLower(gameContext.SystemName), "arcade") {
		return fmt.Sprintf(`Current game context: %s (%s) - This is an arcade game.

User question: %s

Please provide helpful advice about this specific arcade game. Focus on gameplay tips, strategies, or interesting facts about this game.`,
			gameContext.GameName, gameContext.SystemName, message)
	}

	return fmt.Sprintf(`Current game context: %s on %s

User question: %s

Please provide helpful advice about this specific game. Focus on gameplay tips, strategies, or interesting facts.`,
		gameContext.GameName, gameContext.SystemName, message)
}

// buildSuggestionsPrompt creates a prompt for generating game suggestions
func (c *Client) buildSuggestionsPrompt(gameContext *GameContext) string {
	if gameContext == nil || gameContext.GameName == "" {
		return "Generate 3 brief gaming tips for retro game enthusiasts."
	}

	return fmt.Sprintf(`Current game: %s (%s)

Generate exactly 3 brief, helpful suggestions for playing this specific game. Each suggestion should be:
- One clear sentence
- Actionable and specific to this game
- About gameplay strategy, tips, or interesting mechanics

Format as a simple numbered list:
1. [suggestion]
2. [suggestion]  
3. [suggestion]`,
		gameContext.GameName, gameContext.SystemName)
}

// parseSuggestions extracts suggestions from Claude's response
func (c *Client) parseSuggestions(content string) []string {
	lines := strings.Split(content, "\n")
	var suggestions []string

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Remove list numbering
		part := strings.TrimPrefix(line, "1. ")
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

// buildActiveGamePlaylistPrompt creates a prompt for generating playlists based on current game
func (c *Client) buildActiveGamePlaylistPrompt(request *PlaylistRequest, gameContext *GameContext) string {
	basePrompt := fmt.Sprintf(`You're helping a retro gaming enthusiast discover new games. They're currently playing "%s" on %s and want recommendations for similar games.

Generate %d game recommendations that share similar qualities with their current game. Focus on:
- Similar gameplay mechanics or genre
- Games from the same era or system family  
- Games with comparable difficulty or style

Theme: %s

Format your response as a simple list of game names, one per line. Only include actual game names that exist.`,
		gameContext.GameName, gameContext.SystemName, request.GameCount, request.Theme)

	if len(request.Systems) > 0 {
		basePrompt += fmt.Sprintf("\n\nFocus recommendations on these systems: %s", strings.Join(request.Systems, ", "))
	}

	return basePrompt
}

// parseGameRecommendations extracts game names from Claude's playlist response
func (c *Client) parseGameRecommendations(content string, maxGames int, installedGames []InstalledGame) []GameSuggestion {
	lines := strings.Split(content, "\n")
	var games []GameSuggestion
	installed := make(map[string]bool)

	// Create lookup for installed games
	for _, game := range installedGames {
		installed[strings.ToLower(game.Name)] = true
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Remove list markers
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "* ")
		for i := 1; i <= 20; i++ {
			line = strings.TrimPrefix(line, fmt.Sprintf("%d. ", i))
		}

		line = strings.TrimSpace(line)

		if len(line) >= 3 && len(line) <= 100 {
			isInstalled := installed[strings.ToLower(line)]
			games = append(games, GameSuggestion{
				Name:        line,
				IsInstalled: isInstalled,
			})
		}

		if len(games) >= maxGames {
			break
		}
	}

	return games
}

// getOrCreateSession retrieves or creates a chat session
func (c *Client) getOrCreateSession(sessionID string) *ChatSession {
	c.sessionMux.Lock()
	defer c.sessionMux.Unlock()

	if sessionID == "" {
		sessionID = "default"
	}

	session, exists := c.sessions[sessionID]
	if !exists {
		session = &ChatSession{
			Messages: make([]AnthropicMessage, 0),
			Created:  time.Now(),
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

// BuildGameContext extracts current game information from tracker
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

	// ✅ PRIORITY 1: Check if SAM is active (with improved detection)
	if samInfo := c.checkSAMStatus(); samInfo != nil {
		c.logger.Info("claude debug: SAM detected, using SAM game info")
		context.GameName = samInfo.GameName
		context.CoreName = samInfo.CoreName
		context.SystemName = samInfo.SystemName
		return context
	}

	// ✅ PRIORITY 2: Fix arcade detection when SAM is NOT active
	if context.CoreName != "" {
		c.logger.Info("claude debug: SAM not active, checking arcade detection for core '%s'", context.CoreName)

		// ✅ IMPROVED: Always check if it's arcade, regardless of what tracker says
		if arcadeName := c.extractArcadeGameName(context.CoreName); arcadeName != "" {
			c.logger.Info("claude debug: ✅ ARCADE DETECTED: Core '%s' -> Game '%s'", context.CoreName, arcadeName)
			context.GameName = arcadeName
			context.SystemName = "Arcade"
			context.GamePath = ""
			return context
		}

		// ✅ IMPROVED: Fallback for known arcade cores without MRA match
		if c.isLikelyArcadeCore(context.CoreName) {
			c.logger.Info("claude debug: ✅ ARCADE FALLBACK: Core '%s' treated as arcade", context.CoreName)
			context.GameName = context.CoreName
			context.SystemName = "Arcade"
			context.GamePath = ""
			return context
		}

		c.logger.Info("claude debug: ✅ NON-ARCADE: Core '%s' is not arcade, keeping tracker data", context.CoreName)
	}

	c.logger.Info("claude debug: === FINAL CONTEXT ===")
	c.logger.Info("claude debug: Final GameName = '%s'", context.GameName)
	c.logger.Info("claude debug: Final SystemName = '%s'", context.SystemName)
	c.logger.Info("claude debug: Final CoreName = '%s'", context.CoreName)

	return context
}

// ✅ MODIFIED: Main function to use samindex first
func (c *Client) extractArcadeGameName(coreName string) string {
	c.logger.Info("claude debug: === EXTRACTING ARCADE NAME ===")
	c.logger.Info("claude debug: Looking for arcade name for core: '%s'", coreName)

	// 🎯 PRIORITY 1: Use samindex directly (like SAM does)
	if name := c.extractArcadeGameNameUsingSamindex(coreName); name != "" {
		c.logger.Info("claude debug: ✅ SUCCESS via samindex: '%s' -> '%s'", coreName, name)
		return name
	}

	// 🎯 PRIORITY 2: Fallback to previous method if samindex fails
	c.logger.Info("claude debug: ⚠️ samindex failed, trying fallback method")
	if name := c.extractArcadeGameNameFallback(coreName); name != "" {
		c.logger.Info("claude debug: ✅ SUCCESS via fallback: '%s' -> '%s'", coreName, name)
		return name
	}

	// 🎯 PRIORITY 3: Last resort - cleaned core name
	if c.isLikelyArcadeCore(coreName) {
		cleaned := c.cleanCoreName(coreName)
		c.logger.Info("claude debug: ✅ LAST RESORT: Using cleaned core name '%s' -> '%s'", coreName, cleaned)
		return cleaned
	}

	c.logger.Info("claude debug: ❌ COMPLETE FAILURE: No arcade name found for core '%s'", coreName)
	return ""
}

// ✅ NEW IMPLEMENTATION: Use samindex directly like SAM does
func (c *Client) extractArcadeGameNameUsingSamindex(coreName string) string {
	c.logger.Info("claude debug: === USING SAMINDEX DIRECTLY ===")
	c.logger.Info("claude debug: Looking for arcade game for core: '%s'", coreName)

	// 1. Define paths like SAM does
	mrsampath := "/media/fat/Scripts/.MiSTer_SAM"
	samindexBinary := filepath.Join(mrsampath, "samindex")
	tempGamelistPath := "/tmp"
	gamelistFile := filepath.Join(tempGamelistPath, "arcade_gamelist.txt")

	// 2. Verify samindex exists
	if _, err := os.Stat(samindexBinary); err != nil {
		c.logger.Error("claude debug: ❌ samindex not found at %s: %s", samindexBinary, err)
		return ""
	}

	// 3. Execute samindex exactly like SAM: samindex -s arcade -o /tmp
	c.logger.Info("claude debug: 🔧 Executing: %s -q -s arcade -o %s", samindexBinary, tempGamelistPath)
	cmd := exec.Command(samindexBinary, "-q", "-s", "arcade", "-o", tempGamelistPath)

	// Execute the command
	output, err := cmd.CombinedOutput()
	if err != nil {
		c.logger.Error("claude debug: ❌ samindex execution failed: %s, output: %s", err, string(output))
		return ""
	}

	c.logger.Info("claude debug: ✅ samindex executed successfully")

	// 4. Read the generated gamelist file
	if _, err := os.Stat(gamelistFile); err != nil {
		c.logger.Error("claude debug: ❌ Gamelist file not found: %s", gamelistFile)
		return ""
	}

	content, err := os.ReadFile(gamelistFile)
	if err != nil {
		c.logger.Error("claude debug: ❌ Could not read gamelist file: %s", err)
		return ""
	}

	c.logger.Info("claude debug: 📄 Reading gamelist with %d bytes", len(content))

	// 5. Search for the core in MRA file paths
	lines := strings.Split(string(content), "\n")
	coreNameLower := strings.ToLower(coreName)

	c.logger.Info("claude debug: 🔍 Searching for core '%s' in %d MRA files", coreName, len(lines))

	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Log first 5 lines for debugging
		if i < 5 {
			c.logger.Info("claude debug: Sample MRA line %d: %s", i+1, line)
		}

		// Extract filename without extension
		filename := filepath.Base(line)
		nameWithoutExt := strings.TrimSuffix(filename, ".mra")
		nameWithoutExtLower := strings.ToLower(nameWithoutExt)

		// Look for match with core name
		if strings.Contains(nameWithoutExtLower, coreNameLower) {
			c.logger.Info("claude debug: 🎯 MATCH FOUND: '%s' matches '%s'", coreName, nameWithoutExt)

			// Try to extract full name from MRA file
			if fullName := c.extractGameNameFromMRA(line); fullName != "" {
				c.logger.Info("claude debug: ✅ SUCCESS: Extracted full name from MRA: '%s'", fullName)
				return fullName
			}

			// Fallback to filename
			c.logger.Info("claude debug: ✅ SUCCESS: Using filename: '%s'", nameWithoutExt)
			return nameWithoutExt
		}
	}

	c.logger.Info("claude debug: ❌ No match found for core '%s' in arcade gamelist", coreName)
	return ""
}

// ✅ RENAMED: Previous method as fallback
func (c *Client) extractArcadeGameNameFallback(coreName string) string {
	c.logger.Info("claude debug: === FALLBACK METHOD ===")

	arcadeDir := "/media/fat/_Arcade"

	entries, err := os.ReadDir(arcadeDir)
	if err != nil {
		c.logger.Error("claude debug: ❌ Could not read _Arcade directory: %s", err)
		return ""
	}

	c.logger.Info("claude debug: Found %d entries in _Arcade directory", len(entries))

	coreNameLower := strings.ToLower(coreName)
	bestMatch := ""
	bestMatchPath := ""
	bestScore := 0
	totalMraFiles := 0

	// Find best match by filename
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filename := entry.Name()
		if !strings.HasSuffix(strings.ToLower(filename), ".mra") {
			continue
		}

		totalMraFiles++

		nameWithoutExt := strings.TrimSuffix(filename, ".mra")
		nameWithoutExtLower := strings.ToLower(nameWithoutExt)

		// Extract base name (before first parenthesis)
		baseName := nameWithoutExt
		if idx := strings.Index(nameWithoutExt, "("); idx != -1 {
			baseName = strings.TrimSpace(nameWithoutExt[:idx])
		}
		baseNameLower := strings.ToLower(baseName)

		// Calculate score
		score := c.calculateMatchScore(coreNameLower, baseNameLower, nameWithoutExtLower)

		if score > bestScore {
			bestScore = score
			bestMatch = baseName
			bestMatchPath = filepath.Join(arcadeDir, filename)
		}
	}

	c.logger.Info("claude debug: Processed %d .mra files, best score: %d", totalMraFiles, bestScore)

	if bestScore == 0 {
		return ""
	}

	// Try to extract from XML if we have a match
	if bestMatchPath != "" {
		if xmlName := c.extractGameNameFromMRA(bestMatchPath); xmlName != "" {
			return xmlName
		}
	}

	// Minimum score to accept
	if bestScore >= 50 {
		return bestMatch
	}

	return ""
}

// ✅ NEW: Function to clean core names
func (c *Client) cleanCoreName(coreName string) string {
	// Remove common numbers and special characters
	cleaned := coreName
	cleaned = strings.TrimSuffix(cleaned, "h") // ssf2h -> ssf2
	cleaned = strings.TrimSuffix(cleaned, "t") // ssf2t -> ssf2

	// Convert known abbreviations
	replacements := map[string]string{
		"ssf2": "Super Street Fighter II",
		"sf2":  "Street Fighter II",
		"mk":   "Mortal Kombat",
		"kof":  "King of Fighters",
		"ms":   "Metal Slug",
	}

	if replacement, exists := replacements[strings.ToLower(cleaned)]; exists {
		return replacement
	}

	return cleaned
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

	return float64(commonChars)/float64(len(shorter)) > 0.6
}

// isLikelyArcadeCore determines if a core name suggests an arcade game
func (c *Client) isLikelyArcadeCore(coreName string) bool {
	if coreName == "" {
		return false
	}

	// Known non-arcade systems
	nonArcadeSystems := []string{
		"nes", "snes", "genesis", "megadrive", "n64", "psx", "saturn",
		"gb", "gbc", "gba", "gg", "sms", "tgfx16", "neogeo",
		"atari", "c64", "amiga", "ao486", "minimig",
	}

	coreNameLower := strings.ToLower(coreName)
	for _, system := range nonArcadeSystems {
		if strings.Contains(coreNameLower, system) {
			return false
		}
	}

	// If it's a short name without clear system indicators, likely arcade
	return len(coreName) <= 8 && !strings.Contains(coreName, "/")
}

// ✅ IMPROVED: Simple SAM detection with age check only
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

	// ✅ Check if SAM file is too old (obsolete)
	stat, err := os.Stat(samFile)
	if err != nil {
		c.logger.Info("claude debug: Cannot stat SAM file: %s", err)
		return nil
	}

	age := time.Since(stat.ModTime())
	maxAge := 3 * time.Minute // SAM file older than 3 minutes is considered obsolete

	c.logger.Info("claude debug: SAM file age: %v (max allowed: %v)", age, maxAge)

	if age > maxAge {
		c.logger.Info("claude debug: SAM file is too old (%v) - treating as obsolete", age)
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
		"neogeo":    "SNK NeoGeo",
		"psx":       "Sony PlayStation",
		"saturn":    "Sega Saturn",
		"n64":       "Nintendo 64",
		"gb":        "Game Boy",
		"gbc":       "Game Boy Color",
		"gba":       "Game Boy Advance",
	}

	if system, exists := systemMap[strings.ToLower(coreName)]; exists {
		return system
	}

	// Default to titlecase of core name
	return strings.Title(coreName)
}

// ✅ FINAL FIX: Extract game name from MRA XML with correct patterns
func (c *Client) extractGameNameFromMRA(mraPath string) string {
	content, err := os.ReadFile(mraPath)
	if err != nil {
		c.logger.Error("claude debug: ❌ Could not read MRA file %s: %s", mraPath, err)
		return ""
	}

	contentStr := string(content)

	// Show first 200 chars of MRA content for debugging
	preview := contentStr
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}
	c.logger.Info("claude debug: MRA content preview: %s", preview)

	// ✅ CORRECTED: Try all possible XML name patterns
	// Pattern 1: <name>Game Name</name> (full name tag)
	nameStart := strings.Index(contentStr, "<name>")
	if nameStart != -1 {
		nameStart += 6 // len("<name>")
		nameEnd := strings.Index(contentStr[nameStart:], "</name>")
		if nameEnd != -1 {
			gameName := strings.TrimSpace(contentStr[nameStart : nameStart+nameEnd])
			if gameName != "" {
				c.logger.Info("claude debug: ✅ Extracted from <name> tag: '%s'", gameName)
				return gameName
			}
		}
	}

	// Pattern 2: <n>Game Name</n> (short name tag)
	nameStart = strings.Index(contentStr, "<n>")
	if nameStart != -1 {
		nameStart += 3 // len("<n>")
		nameEnd := strings.Index(contentStr[nameStart:], "</n>")
		if nameEnd != -1 {
			gameName := strings.TrimSpace(contentStr[nameStart : nameStart+nameEnd])
			if gameName != "" {
				c.logger.Info("claude debug: ✅ Extracted from <n> tag: '%s'", gameName)
				return gameName
			}
		}
	}

	// Pattern 3: <setname>Game Name</setname>
	nameStart = strings.Index(contentStr, "<setname>")
	if nameStart != -1 {
		nameStart += 9 // len("<setname>")
		nameEnd := strings.Index(contentStr[nameStart:], "</setname>")
		if nameEnd != -1 {
			gameName := strings.TrimSpace(contentStr[nameStart : nameStart+nameEnd])
			if gameName != "" {
				c.logger.Info("claude debug: ✅ Extracted from <setname> tag: '%s'", gameName)
				return gameName
			}
		}
	}

	c.logger.Info("claude debug: ❌ No name tags found in MRA XML (tried: name, n, setname)")
	return ""
}

// Helper function for min (add at the end of the file if it doesn't exist)
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
