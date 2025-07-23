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

	session := c.getOrCreateSession(sessionID)
	session.Updated = time.Now()

	prompt := c.buildContextualPrompt(message, gameContext)

	messages := make([]Message, len(session.Messages))
	copy(messages, session.Messages)

	messages = append(messages, Message{
		Role:    "user",
		Content: prompt,
	})

	reqBody := AnthropicRequest{
		Model:     "claude-3-5-sonnet-20240620",
		MaxTokens: maxTokens,
		Messages:  messages,
	}

	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		c.logger.Error("claude marshal request: %s", err)
		return &ChatResponse{
			Error:     "Failed to create request",
			Timestamp: time.Now(),
		}, nil
	}

	req, err := http.NewRequestWithContext(ctx, "POST", anthropicAPIURL, bytes.NewBuffer(reqJSON))
	if err != nil {
		c.logger.Error("claude create request: %s", err)
		return &ChatResponse{
			Error:     "Failed to create request",
			Timestamp: time.Now(),
		}, nil
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.config.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("User-Agent", userAgent)

	c.logger.Info("claude sending request to API")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Error("claude API request failed: %s", err)
		return &ChatResponse{
			Error:     "Request failed",
			Timestamp: time.Now(),
		}, nil
	}
	defer resp.Body.Close()

	var apiResp AnthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		c.logger.Error("claude decode response: %s", err)
		return &ChatResponse{
			Error:     "Failed to decode response",
			Timestamp: time.Now(),
		}, nil
	}

	if resp.StatusCode != http.StatusOK {
		c.logger.Error("claude API error %d: %s", resp.StatusCode, apiResp.Error.Message)
		return &ChatResponse{
			Error:     fmt.Sprintf("API error: %s", apiResp.Error.Message),
			Timestamp: time.Now(),
		}, nil
	}

	if len(apiResp.Content) == 0 {
		c.logger.Error("claude empty response")
		return &ChatResponse{
			Error:     "Empty response",
			Timestamp: time.Now(),
		}, nil
	}

	content := apiResp.Content[0].Text
	c.logger.Info("claude response received: %d characters", len(content))

	session.Messages = append(session.Messages, Message{
		Role:    "user",
		Content: prompt,
	}, Message{
		Role:    "assistant",
		Content: content,
	})

	c.trimSessionHistory(session)

	return &ChatResponse{
		Content:   content,
		Timestamp: time.Now(),
	}, nil
}

// GenerateSuggestions generates contextual suggestions for the current game
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
		part = strings.TrimPrefix(part, "- ")
		part = strings.TrimSpace(part)

		if part != line && len(part) >= 10 && len(part) <= 200 {
			suggestions = append(suggestions, part)
		}

		if len(suggestions) >= 3 {
			break
		}
	}

	return suggestions
}

// parseGameRecommendations parses Claude's response into game recommendations
func (c *Client) parseGameRecommendations(content string, maxGames int) []GameRecommendation {
	lines := strings.Split(content, "\n")
	games := make([]GameRecommendation, 0)

	for i, line := range lines {
		if len(games) >= maxGames {
			break
		}

		if strings.Contains(line, "---") || strings.Contains(line, "===") {
			continue
		}

		line = strings.TrimSpace(line)
		for _, prefix := range []string{"1. ", "2. ", "3. ", "4. ", "5. ", "6. ", "7. ", "8. ", "9. ", "10. ", "- "} {
			line = strings.TrimPrefix(line, prefix)
		}
		line = strings.TrimPrefix(line, fmt.Sprintf("%d. ", i))

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
		CoreName:   trk.ActiveCore,
		SystemName: c.mapCoreToSystem(trk.ActiveCore),
	}

	// ✅ STEP 1: Handle arcade games with new samindex method
	if c.isLikelyArcadeCore(trk.ActiveCore) {
		c.logger.Info("claude debug: 🎮 ARCADE GAME DETECTED")
		context.SystemName = "Arcade"

		// Try to get game name from samindex first
		if arcadeName := c.extractArcadeGameName(trk.ActiveCore); arcadeName != "" {
			context.GameName = arcadeName
			c.logger.Info("claude debug: ✅ Using arcade name: '%s'", arcadeName)
		} else {
			// Fallback to cleaned core name
			context.GameName = c.cleanCoreName(trk.ActiveCore)
			c.logger.Info("claude debug: ⚠️ Using cleaned core name: '%s'", context.GameName)
		}
	} else {
		// ✅ STEP 2: Handle non-arcade games (existing logic)
		c.logger.Info("claude debug: 🎮 NON-ARCADE GAME")

		if trk.ActiveGameName != "" {
			context.GameName = trk.ActiveGameName
			c.logger.Info("claude debug: ✅ Using tracker game name: '%s'", context.GameName)
		} else if trk.ActiveGame != "" {
			context.GameName = c.cleanFileName(trk.ActiveGame)
			c.logger.Info("claude debug: ✅ Using cleaned ActiveGame: '%s'", context.GameName)
		} else {
			context.GameName = c.cleanCoreName(trk.ActiveCore)
			c.logger.Info("claude debug: ⚠️ Using core name as fallback: '%s'", context.GameName)
		}
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

	// 5. Parse the gamelist to find our core
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// SAM gamelist format: /path/to/core.rbf:Game Name
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		path := parts[0]
		gameName := parts[1]

		// Extract core name from path
		coreFile := filepath.Base(path)
		coreNameFromPath := strings.TrimSuffix(coreFile, filepath.Ext(coreFile))

		// Check if this matches our core
		if strings.EqualFold(coreNameFromPath, coreName) {
			c.logger.Info("claude debug: ✅ MATCH FOUND: core '%s' -> game '%s'", coreName, gameName)
			return gameName
		}
	}

	c.logger.Info("claude debug: ❌ No match found in samindex gamelist for core '%s'", coreName)
	return ""
}

// ✅ PREVIOUS METHOD: Fallback if samindex fails
func (c *Client) extractArcadeGameNameFallback(coreName string) string {
	c.logger.Info("claude debug: === USING FALLBACK METHOD ===")

	// Try different arcade paths
	arcadePaths := []string{
		"/media/fat/_Arcade",
		"/media/fat/_Arcade/cores",
	}

	for _, arcadePath := range arcadePaths {
		if _, err := os.Stat(arcadePath); err != nil {
			continue
		}

		// Look for MRA files that might correspond to this core
		mraPattern := filepath.Join(arcadePath, "*.mra")
		matches, err := filepath.Glob(mraPattern)
		if err != nil {
			continue
		}

		c.logger.Info("claude debug: 🔍 Found %d MRA files in %s", len(matches), arcadePath)

		for _, mraPath := range matches {
			if gameName := c.extractGameNameFromMRA(mraPath); gameName != "" {
				// Simple heuristic: if the core name appears in the game name or MRA filename
				mraFile := strings.ToLower(filepath.Base(mraPath))
				if strings.Contains(mraFile, strings.ToLower(coreName)) ||
					strings.Contains(strings.ToLower(gameName), strings.ToLower(coreName)) {
					c.logger.Info("claude debug: ✅ Found matching MRA: %s -> %s", mraPath, gameName)
					return gameName
				}
			}
		}
	}

	return ""
}

// isLikelyArcadeCore determines if a core is probably an arcade core
func (c *Client) isLikelyArcadeCore(coreName string) bool {
	if coreName == "" {
		return false
	}

	// Common patterns for arcade cores
	lowerCore := strings.ToLower(coreName)

	// Known arcade core patterns
	arcadePatterns := []string{
		"dkong", "pacman", "galaga", "frogger", "defender", "asteroids",
		"centipede", "qbert", "joust", "robotron", "tempest", "missile",
		"burgertime", "digdug", "mappy", "xevious", "bosconian",
		"rallyx", "pengo", "crush", "ladybug", "mrdo", "bagman",
		"phoenix", "pleiads", "scramble", "frenzy", "carnival",
		"amidar", "tutankham", "jungler", "locomotion", "checkman",
		"mooncresta", "galaxian", "uniwars", "zigzag", "jrpacman",
	}

	for _, pattern := range arcadePatterns {
		if strings.Contains(lowerCore, pattern) {
			return true
		}
	}

	// Check for typical arcade core naming patterns
	// Most arcade cores are short alphanumeric names, often 3-8 characters
	if len(coreName) >= 3 && len(coreName) <= 8 {
		// If it's all lowercase and contains numbers, likely arcade
		hasLetter := false
		hasNumber := false
		for _, r := range lowerCore {
			if r >= 'a' && r <= 'z' {
				hasLetter = true
			}
			if r >= '0' && r <= '9' {
				hasNumber = true
			}
		}
		if hasLetter && (hasNumber || len(coreName) <= 6) {
			return true
		}
	}

	return false
}

// cleanCoreName converts technical core names to readable names
func (c *Client) cleanCoreName(coreName string) string {
	if coreName == "" {
		return "Unknown Game"
	}

	// Common core name mappings for better readability
	nameMap := map[string]string{
		"dkong":     "Donkey Kong",
		"dkong3":    "Donkey Kong 3",
		"dkongjr":   "Donkey Kong Jr.",
		"pacman":    "Pac-Man",
		"mspacman":  "Ms. Pac-Man",
		"galaga":    "Galaga",
		"galaxian":  "Galaxian",
		"frogger":   "Frogger",
		"defender":  "Defender",
		"stargate":  "Stargate",
		"robotron":  "Robotron 2084",
		"joust":     "Joust",
		"bubbles":   "Bubbles",
		"splat":     "Splat",
		"asteroids": "Asteroids",
		"asteroid":  "Asteroids",
		"tempest":   "Tempest",
		"missile":   "Missile Command",
		"centipede": "Centipede",
		"millipede": "Millipede",
		"qbert":     "Q*bert",
		"ssf2":      "Super Street Fighter II",
		"ssf2t":     "Super Street Fighter II Turbo",
		"ssf2h":     "Super Street Fighter II",
		"sf2":       "Street Fighter II",
		"sf2ce":     "Street Fighter II Champion Edition",
		"kof94":     "The King of Fighters '94",
		"kof95":     "The King of Fighters '95",
		"kof96":     "The King of Fighters '96",
		"kof97":     "The King of Fighters '97",
		"kof98":     "The King of Fighters '98",
		"kof99":     "The King of Fighters '99",
		"kof2000":   "The King of Fighters 2000",
		"kof2001":   "The King of Fighters 2001",
		"kof2002":   "The King of Fighters 2002",
		"kof2003":   "The King of Fighters 2003",
		"mslug":     "Metal Slug",
		"mslug2":    "Metal Slug 2",
		"mslug3":    "Metal Slug 3",
		"mslug4":    "Metal Slug 4",
		"mslug5":    "Metal Slug 5",
		"mslugx":    "Metal Slug X",
		"neogeo":    "Neo Geo",
		"mvsc":      "Marvel vs. Capcom",
		"mvsc2":     "Marvel vs. Capcom 2",
		"tekken":    "Tekken",
		"tekken2":   "Tekken 2",
		"tekken3":   "Tekken 3",
	}

	lowerCore := strings.ToLower(coreName)
	if mapped, exists := nameMap[lowerCore]; exists {
		return mapped
	}

	// Basic cleanup: replace underscores with spaces and title case
	cleaned := strings.ReplaceAll(coreName, "_", " ")
	cleaned = strings.ReplaceAll(cleaned, "-", " ")

	// Title case each word
	words := strings.Fields(cleaned)
	for i, word := range words {
		if len(word) > 0 {
			words[i] = strings.ToUpper(word[:1]) + strings.ToLower(word[1:])
		}
	}

	result := strings.Join(words, " ")
	if result == "" {
		return coreName // Return original if cleaning failed
	}

	return result
}

// cleanFileName removes extensions and cleans up filenames
func (c *Client) cleanFileName(filename string) string {
	if filename == "" {
		return ""
	}

	// Remove path and extension
	base := filepath.Base(filename)
	name := strings.TrimSuffix(base, filepath.Ext(base))

	// Remove common suffixes like (USA), [!], etc.
	name = strings.ReplaceAll(name, "_", " ")
	name = strings.ReplaceAll(name, "-", " ")

	// Remove parentheses and brackets content
	for {
		start := strings.Index(name, "(")
		if start == -1 {
			break
		}
		end := strings.Index(name[start:], ")")
		if end == -1 {
			break
		}
		name = name[:start] + name[start+end+1:]
	}

	for {
		start := strings.Index(name, "[")
		if start == -1 {
			break
		}
		end := strings.Index(name[start:], "]")
		if end == -1 {
			break
		}
		name = name[:start] + name[start+end+1:]
	}

	// Clean up extra spaces
	name = strings.TrimSpace(name)
	for strings.Contains(name, "  ") {
		name = strings.ReplaceAll(name, "  ", " ")
	}

	return name
}

// mapCoreToSystem maps core names to friendly system names
func (c *Client) mapCoreToSystem(coreName string) string {
	systemMap := map[string]string{
		"nes":     "Nintendo Entertainment System",
		"snes":    "Super Nintendo",
		"genesis": "Sega Genesis",
		"megacd":  "Sega CD",
		"s32x":    "Sega 32X",
		"arcade":  "Arcade",
		"neogeo":  "SNK NeoGeo",
		"psx":     "Sony PlayStation",
		"saturn":  "Sega Saturn",
		"n64":     "Nintendo 64",
		"gb":      "Game Boy",
		"gbc":     "Game Boy Color",
		"gba":     "Game Boy Advance",
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

// GeneratePlaylistFromActiveGame generates game recommendations based on current active game
func (c *Client) GeneratePlaylistFromActiveGame(ctx context.Context, request *PlaylistRequest, trk *tracker.Tracker) (*PlaylistResponse, error) {
	if !c.config.Enabled {
		return &PlaylistResponse{
			Error:     "Claude is disabled",
			Timestamp: time.Now(),
		}, nil
	}

	// Build game context from tracker
	gameContext := c.BuildGameContext(trk)
	if gameContext.GameName == "" {
		return &PlaylistResponse{
			Error:     "No active game to base recommendations on",
			Timestamp: time.Now(),
		}, nil
	}

	// Build prompt based on active game
	prompt := c.buildActiveGamePlaylistPrompt(request, gameContext)

	// Send request to Claude
	response, err := c.SendMessage(ctx, prompt, gameContext, "playlist")
	if err != nil {
		return &PlaylistResponse{
			Error:     "Failed to generate playlist",
			Timestamp: time.Now(),
		}, nil
	}

	if response.Error != "" {
		return &PlaylistResponse{
			Error:     response.Error,
			Timestamp: time.Now(),
		}, nil
	}

	// Parse games from response
	games := c.parseGameRecommendations(response.Content, request.GameCount)

	return &PlaylistResponse{
		Games:     games,
		Theme:     request.Theme,
		Timestamp: time.Now(),
	}, nil
}

// GeneratePlaylist generates game recommendations based on theme
func (c *Client) GeneratePlaylist(ctx context.Context, request *PlaylistRequest) (*PlaylistResponse, error) {
	if !c.config.Enabled {
		return &PlaylistResponse{
			Error:     "Claude is disabled",
			Timestamp: time.Now(),
		}, nil
	}

	// Build general theme prompt
	prompt := c.buildGeneralPlaylistPrompt(request)

	// Send request to Claude
	response, err := c.SendMessage(ctx, prompt, nil, "playlist")
	if err != nil {
		return &PlaylistResponse{
			Error:     "Failed to generate playlist",
			Timestamp: time.Now(),
		}, nil
	}

	if response.Error != "" {
		return &PlaylistResponse{
			Error:     response.Error,
			Timestamp: time.Now(),
		}, nil
	}

	// Parse games from response
	games := c.parseGameRecommendations(response.Content, request.GameCount)

	return &PlaylistResponse{
		Games:     games,
		Theme:     request.Theme,
		Timestamp: time.Now(),
	}, nil
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

// buildGeneralPlaylistPrompt creates a prompt for general theme-based playlists
func (c *Client) buildGeneralPlaylistPrompt(request *PlaylistRequest) string {
	basePrompt := fmt.Sprintf(`Generate %d retro game recommendations based on this theme: "%s"

Focus on well-known classic games that fit the theme. Consider:
- Gameplay mechanics and genre
- Historical significance
- Quality and reputation
- Variety across different systems

Format your response as a simple list of game names, one per line. Only include actual game names that exist.`,
		request.GameCount, request.Theme)

	if len(request.Systems) > 0 {
		basePrompt += fmt.Sprintf("\n\nLimit recommendations to these systems: %s", strings.Join(request.Systems, ", "))
	}

	if request.Preferences != "" {
		basePrompt += fmt.Sprintf("\n\nUser preferences: %s", request.Preferences)
	}

	return basePrompt
}

// Helper function for min
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
