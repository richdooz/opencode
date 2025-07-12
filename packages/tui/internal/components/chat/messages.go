package chat

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/v2/viewport"
	tea "github.com/charmbracelet/bubbletea/v2"
	"github.com/charmbracelet/lipgloss/v2"
	"github.com/sst/opencode-sdk-go"
	"github.com/sst/opencode/internal/app"
	"github.com/sst/opencode/internal/components/dialog"
	"github.com/sst/opencode/internal/layout"
	"github.com/sst/opencode/internal/styles"
	"github.com/sst/opencode/internal/theme"
	"github.com/sst/opencode/internal/util"
)

type MessagesComponent interface {
	tea.Model
	View(width, height int) string
	SetWidth(width int) tea.Cmd
	PageUp() (tea.Model, tea.Cmd)
	PageDown() (tea.Model, tea.Cmd)
	HalfPageUp() (tea.Model, tea.Cmd)
	HalfPageDown() (tea.Model, tea.Cmd)
	First() (tea.Model, tea.Cmd)
	Last() (tea.Model, tea.Cmd)
	Previous() (tea.Model, tea.Cmd)
	Next() (tea.Model, tea.Cmd)
	ToolDetailsVisible() bool
	Selected() string
}

// MessageInfo holds metadata about each message for virtualization
type MessageInfo struct {
	Height         int
	YOffset        int
	CacheKey       string
	LastWidth      int
	NeedsUpdate    bool
	LastTheme      string // Track theme changes (future optimization)
	LastToolDetails bool   // Track tool details changes
}

type messagesComponent struct {
	width           int
	app             *app.App
	viewport        viewport.Model
	cache           *MessageCache
	rendering       bool
	showToolDetails bool
	tail            bool
	partCount       int
	lineCount       int
	selectedPart    int
	selectedText    string

	// Virtualization state
	messageInfos    []MessageInfo
	totalHeight     int
	visibleStart    int
	visibleEnd      int
	bufferSize      int  // Messages to render outside viewport for smooth scrolling
	lastMessageCount int
	lastWidth       int
	needsRecalculation bool
	
	// Render throttling
	renderPending   bool
	lastRenderTime  time.Time
	renderThrottle  time.Duration
	
	// Orphaned tool calls state (for interim output)  
	// These represent tool calls that occur without associated text (planning, tool usage, etc.)
	orphanedToolCalls []opencode.ToolPart
}

type renderFinishedMsg struct{}
type selectedMessagePartChangedMsg struct {
	part int
}

type ToggleToolDetailsMsg struct{}

type throttledRenderMsg struct {
	width int
}

func (m *messagesComponent) Init() tea.Cmd {
	return tea.Batch(m.viewport.Init())
}

func (m *messagesComponent) Selected() string {
	return m.selectedText
}

func (m *messagesComponent) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case app.SendMsg:
		m.viewport.GotoBottom()
		m.tail = true
		m.selectedPart = -1
		return m, nil
	case app.OptimisticMessageAddedMsg:
		m.tail = true
		m.rendering = true
		m.invalidateMessageInfos()
		return m, m.Reload()
	case dialog.ThemeSelectedMsg:
		// Instead of clearing entire cache, just invalidate rendering
		m.rendering = true
		m.invalidateMessageInfos()
		return m, m.Reload()
	case ToggleToolDetailsMsg:
		m.showToolDetails = !m.showToolDetails
		m.rendering = true
		m.invalidateMessageInfos()
		return m, m.Reload()
	case app.SessionLoadedMsg, app.SessionClearedMsg:
		m.cache.Clear()
		m.tail = true
		m.rendering = true
		m.invalidateMessageInfos()
		return m, m.Reload()
	case renderFinishedMsg:
		m.rendering = false
		if m.tail {
			m.viewport.GotoBottom()
		}
	case selectedMessagePartChangedMsg:
		return m, m.Reload()
	case throttledRenderMsg:
		m.renderPending = false
		m.renderView(msg.width)
		if m.tail {
			m.viewport.GotoBottom()
		}
	case opencode.EventListResponseEventSessionUpdated:
		if msg.Properties.Info.ID == m.app.Session.ID {
			cmd := m.throttledRenderView(m.width)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			if m.tail {
				m.viewport.GotoBottom()
			}
		}
	case opencode.EventListResponseEventMessageUpdated:
		if msg.Properties.Info.SessionID == m.app.Session.ID {
			cmd := m.throttledRenderView(m.width)
			if cmd != nil {
				cmds = append(cmds, cmd)
			}
			if m.tail {
				m.viewport.GotoBottom()
			}
		}
	}

	viewport, cmd := m.viewport.Update(msg)
	m.viewport = viewport
	m.tail = m.viewport.AtBottom()
	cmds = append(cmds, cmd)

	return m, tea.Batch(cmds...)
}

// invalidateMessageInfos marks all message info as needing updates
func (m *messagesComponent) invalidateMessageInfos() {
	for i := range m.messageInfos {
		m.messageInfos[i].NeedsUpdate = true
	}
	m.needsRecalculation = true
}

// invalidateForThemeChange marks messages that need re-rendering due to theme change
func (m *messagesComponent) invalidateForThemeChange() {
	// For now, just invalidate all messages on theme change
	// TODO: Could be optimized further with theme fingerprinting
	for i := range m.messageInfos {
		m.messageInfos[i].NeedsUpdate = true
	}
	m.needsRecalculation = true
}

// invalidateForToolDetailsChange marks messages that need re-rendering due to tool details change
func (m *messagesComponent) invalidateForToolDetailsChange() {
	for i := range m.messageInfos {
		if m.messageInfos[i].LastToolDetails != m.showToolDetails {
			m.messageInfos[i].NeedsUpdate = true
			m.messageInfos[i].LastToolDetails = m.showToolDetails
		}
	}
	m.needsRecalculation = true
}

// calculateVisibleRange determines which messages are currently visible
func (m *messagesComponent) calculateVisibleRange() {
	if len(m.app.Messages) == 0 {
		m.visibleStart = 0
		m.visibleEnd = 0
		return
	}

	viewportTop := m.viewport.YOffset
	viewportBottom := viewportTop + m.viewport.Height()

	// Find first visible message
	m.visibleStart = 0
	for i, info := range m.messageInfos {
		if info.YOffset+info.Height > viewportTop {
			m.visibleStart = i
			break
		}
	}

	// Find last visible message  
	m.visibleEnd = len(m.messageInfos) - 1
	for i := m.visibleStart; i < len(m.messageInfos); i++ {
		if m.messageInfos[i].YOffset > viewportBottom {
			m.visibleEnd = i - 1
			break
		}
	}

	// Add buffer for smooth scrolling
	m.visibleStart = max(0, m.visibleStart-m.bufferSize)
	m.visibleEnd = min(len(m.messageInfos)-1, m.visibleEnd+m.bufferSize)
}

// ensureMessageInfos ensures we have MessageInfo for all messages
func (m *messagesComponent) ensureMessageInfos() {
	messageCount := len(m.app.Messages)
	if len(m.messageInfos) < messageCount {
		// Extend slice
		for len(m.messageInfos) < messageCount {
			m.messageInfos = append(m.messageInfos, MessageInfo{NeedsUpdate: true})
		}
	} else if len(m.messageInfos) > messageCount {
		// Shrink slice
		m.messageInfos = m.messageInfos[:messageCount]
	}
}

// calculateMessageHeights efficiently calculates heights for messages that need it
func (m *messagesComponent) calculateMessageHeights() {
	measure := util.Measure("virtualized.calculateMessageHeights")
	defer measure("messageCount", len(m.app.Messages))

	// Check if we need to recalculate
	messageCountChanged := len(m.app.Messages) != m.lastMessageCount
	widthChanged := m.width != m.lastWidth

	if !m.needsRecalculation && !messageCountChanged && !widthChanged {
		return
	}

	m.lastMessageCount = len(m.app.Messages)
	m.lastWidth = m.width
	m.needsRecalculation = false

	m.ensureMessageInfos()
	
	currentOffset := 0
	for i, message := range m.app.Messages {
		info := &m.messageInfos[i]
		
		// Skip if height is already calculated and still valid
		if !info.NeedsUpdate && info.LastWidth == m.width && info.Height > 0 {
			info.YOffset = currentOffset
			currentOffset += info.Height + 2 // spacing
			continue
		}

		// Calculate height for this message
		height := m.calculateSingleMessageHeight(message, i)
		info.Height = height
		info.YOffset = currentOffset
		info.LastWidth = m.width
		info.NeedsUpdate = false
		
		currentOffset += height + 2 // spacing
	}
	
	m.totalHeight = currentOffset
}

// calculateSingleMessageHeight calculates height for a single message
func (m *messagesComponent) calculateSingleMessageHeight(message opencode.MessageUnion, messageIndex int) int {
	// Use a simpler height estimation to avoid full rendering
	heightCacheKey := m.cache.GenerateKey("height", fmt.Sprintf("%v", message), m.width, m.showToolDetails)
	if heightStr, cached := m.cache.Get(heightCacheKey); cached {
		var height int
		if _, err := fmt.Sscanf(heightStr, "%d", &height); err == nil {
			return height
		}
	}

	// Render to calculate height (this is still expensive but only for uncached messages)
	content := m.renderSingleMessage(message, messageIndex, true)
	height := lipgloss.Height(content)
	
	// Cache height
	m.cache.Set(heightCacheKey, fmt.Sprintf("%d", height))
	return height
}

// throttledRenderView schedules a render with throttling
func (m *messagesComponent) throttledRenderView(width int) tea.Cmd {
	if m.renderPending {
		return nil
	}
	
	now := time.Now()
	sinceLastRender := now.Sub(m.lastRenderTime)
	
	if sinceLastRender < m.renderThrottle {
		// Schedule delayed render
		m.renderPending = true
		return tea.Tick(m.renderThrottle-sinceLastRender, func(time.Time) tea.Msg {
			return throttledRenderMsg{width: width}
		})
	}
	
	// Render immediately
	m.renderView(width)
	return nil
}

// renderView now implements virtualization
func (m *messagesComponent) renderView(width int) {
	measure := util.Measure("virtualized.renderView")
	defer measure("messageCount", len(m.app.Messages))
	
	// Update last render time for throttling
	m.lastRenderTime = time.Now()

	// Skip render if no messages
	if len(m.app.Messages) == 0 {
		m.viewport.SetContent("")
		return
	}

	// Calculate heights and positions
	m.calculateMessageHeights()
	m.calculateVisibleRange()

	// Performance logging
	visibleCount := m.visibleEnd - m.visibleStart + 1
	if visibleCount > 0 {
		util.Measure("virtualized.renderView.visibleCount")("visibleCount", visibleCount)
	}

	blocks := make([]string, 0)
	m.partCount = 0
	m.lineCount = 0

	// Add spacer for messages above visible range
	if m.visibleStart > 0 && len(m.messageInfos) > 0 {
		spacerHeight := m.messageInfos[m.visibleStart].YOffset
		if spacerHeight > 0 {
			spacer := strings.Repeat("\n", spacerHeight-1) // -1 because we add \n between blocks
			blocks = append(blocks, spacer)
			m.lineCount += spacerHeight
		}
	}

	// Process all messages to collect orphaned tool calls
	m.processOrphanedToolCalls()
	
	// Render visible messages
	for i := m.visibleStart; i <= m.visibleEnd && i < len(m.app.Messages); i++ {
		message := m.app.Messages[i]
		content := m.renderSingleMessage(message, i, false)
		
		if content != "" {
			m = m.updateSelected(content, "")
			blocks = append(blocks, content)
		}

		// Handle errors (copied from original logic)
		error := ""
		if assistant, ok := message.(opencode.AssistantMessage); ok {
			switch err := assistant.Error.AsUnion().(type) {
			case nil:
			case opencode.AssistantMessageErrorMessageOutputLengthError:
				error = "Message output length exceeded"
			case opencode.ProviderAuthError:
				error = err.Data.Message
			case opencode.MessageAbortedError:
				error = "Request was aborted"
			case opencode.UnknownError:
				error = err.Data.Message
			}
		}

		if error != "" {
			t := theme.CurrentTheme()
			error = styles.NewStyle().Width(width - 6).Render(error)
			error = renderContentBlock(
				m.app,
				error,
				false,
				width,
				WithBorderColor(t.Error()),
			)
			blocks = append(blocks, error)
			m.lineCount += lipgloss.Height(error) + 1
		}
	}

	// Add spacer for messages below visible range
	if m.visibleEnd < len(m.messageInfos)-1 {
		lastVisibleBottom := m.messageInfos[m.visibleEnd].YOffset + m.messageInfos[m.visibleEnd].Height
		spacerHeight := m.totalHeight - lastVisibleBottom
		if spacerHeight > 0 {
			spacer := strings.Repeat("\n", spacerHeight-1)
			blocks = append(blocks, spacer)
		}
	}

	m.viewport.SetContent("\n" + strings.Join(blocks, "\n\n"))
	if m.selectedPart == m.partCount {
		m.viewport.GotoBottom()
	}
}

// renderSingleMessage renders a single message (simplified version of original complex logic)
func (m *messagesComponent) renderSingleMessage(message opencode.MessageUnion, messageIndex int, heightOnly bool) string {
	orphanedForThisMessage := m.getOrphanedToolCallsForMessage(messageIndex)
	
	switch casted := message.(type) {
	case opencode.UserMessage:
		for partIndex, part := range casted.Parts {
			switch part := part.AsUnion().(type) {
			case opencode.TextPart:
				remainingParts := casted.Parts[partIndex+1:]
				fileParts := make([]opencode.FilePart, 0)
				for _, part := range remainingParts {
					switch part := part.AsUnion().(type) {
					case opencode.FilePart:
						fileParts = append(fileParts, part)
					}
				}

				files := ""
				if len(fileParts) > 0 {
					t := theme.CurrentTheme()
					flexItems := []layout.FlexItem{}
					fileStyle := styles.NewStyle().Background(t.BackgroundElement()).Foreground(t.TextMuted()).Padding(0, 1)
					mediaTypeStyle := styles.NewStyle().Background(t.Secondary()).Foreground(t.BackgroundPanel()).Padding(0, 1)
					for _, filePart := range fileParts {
						mediaType := ""
						switch filePart.Mime {
						case "text/plain":
							mediaType = "txt"
						case "image/png", "image/jpeg", "image/gif", "image/webp":
							mediaType = "img"
							mediaTypeStyle = mediaTypeStyle.Background(t.Accent())
						case "application/pdf":
							mediaType = "pdf"
							mediaTypeStyle = mediaTypeStyle.Background(t.Primary())
						}
						flexItems = append(flexItems, layout.FlexItem{
							View: mediaTypeStyle.Render(mediaType) + fileStyle.Render(filePart.Filename),
						})
					}
					bgColor := t.BackgroundPanel()
					files = layout.Render(
						layout.FlexOptions{
							Background: &bgColor,
							Width:      m.width - 6,
							Direction:  layout.Column,
						},
						flexItems...,
					)
				}

				key := m.cache.GenerateKey(casted.ID, part.Text, m.width, m.selectedPart == m.partCount, files)
				if content, cached := m.cache.Get(key); cached && !heightOnly {
					return content
				}

				content := renderText(
					m.app,
					message,
					part.Text,
					m.app.Info.User,
					m.showToolDetails,
					m.partCount == m.selectedPart,
					m.width,
					files,
				)
				
				if !heightOnly {
					m.cache.Set(key, content)
				}
				return content
			}
		}

	case opencode.AssistantMessage:
		hasTextPart := false
		for partIndex, p := range casted.Parts {
			switch part := p.AsUnion().(type) {
			case opencode.TextPart:
				hasTextPart = true
				finished := casted.Time.Completed > 0
				remainingParts := casted.Parts[partIndex+1:]
				toolCallParts := make([]opencode.ToolPart, 0)

				// Include orphaned tool calls from previous messages
				if len(orphanedForThisMessage) > 0 {
					toolCallParts = append(toolCallParts, orphanedForThisMessage...)
				}

				remaining := true
				for _, part := range remainingParts {
					if !remaining {
						break
					}
					switch part := part.AsUnion().(type) {
					case opencode.TextPart:
						remaining = false
					case opencode.ToolPart:
						toolCallParts = append(toolCallParts, part)
						if part.State.Status != opencode.ToolPartStateStatusCompleted || part.State.Status != opencode.ToolPartStateStatusError {
							finished = false
						}
					}
				}

				if finished && !heightOnly {
					key := m.cache.GenerateKey(casted.ID, p.Text, m.width, m.showToolDetails, m.selectedPart == m.partCount)
					if content, cached := m.cache.Get(key); cached {
						return content
					}
				}

				content := renderText(
					m.app,
					message,
					p.Text,
					casted.ModelID,
					m.showToolDetails,
					m.partCount == m.selectedPart,
					m.width,
					"",
					toolCallParts...,
				)

				if finished && !heightOnly {
					key := m.cache.GenerateKey(casted.ID, p.Text, m.width, m.showToolDetails, m.selectedPart == m.partCount)
					m.cache.Set(key, content)
				}
				return content

			case opencode.ToolPart:
				if !m.showToolDetails {
					if !hasTextPart {
					// Tool calls without text parts are handled in processOrphanedToolCalls
					}
					continue
				}

				if part.State.Status == opencode.ToolPartStateStatusCompleted || part.State.Status == opencode.ToolPartStateStatusError {
					key := m.cache.GenerateKey(casted.ID, part.ID, m.showToolDetails, m.width, m.partCount == m.selectedPart)
					if content, cached := m.cache.Get(key); cached && !heightOnly {
						return content
					}

					content := renderToolDetails(m.app, part, m.partCount == m.selectedPart, m.width)
					if !heightOnly {
						m.cache.Set(key, content)
					}
					return content
				} else {
					return renderToolDetails(m.app, part, m.partCount == m.selectedPart, m.width)
				}
			}
		}
	}

	return ""
}

func (m *messagesComponent) updateSelected(content string, selectedText string) *messagesComponent {
	if m.selectedPart == m.partCount {
		m.viewport.SetYOffset(m.lineCount - (m.viewport.Height() / 2) + 4)
		m.selectedText = selectedText
	}
	m.partCount++
	m.lineCount += lipgloss.Height(content) + 1
	return m
}

func (m *messagesComponent) header(width int) string {
	if m.app.Session.ID == "" {
		return ""
	}

	t := theme.CurrentTheme()
	base := styles.NewStyle().Foreground(t.Text()).Background(t.Background()).Render
	muted := styles.NewStyle().Foreground(t.TextMuted()).Background(t.Background()).Render
	headerLines := []string{}
	headerLines = append(
		headerLines,
		util.ToMarkdown("# "+m.app.Session.Title, width-6, t.Background()),
	)

	share := ""
	if m.app.Session.Share.URL != "" {
		share = muted(m.app.Session.Share.URL + "  /unshare")
	} else {
		share = base("/share") + muted(" to create a shareable link")
	}

	sessionInfo := ""
	tokens := float64(0)
	cost := float64(0)
	contextWindow := m.app.Model.Limit.Context

	for _, message := range m.app.Messages {
		if assistant, ok := message.(opencode.AssistantMessage); ok {
			cost += assistant.Cost
			usage := assistant.Tokens
			if usage.Output > 0 {
				if assistant.Summary {
					tokens = usage.Output
					continue
				}
				tokens = (usage.Input +
					usage.Cache.Write +
					usage.Cache.Read +
					usage.Output +
					usage.Reasoning)
			}
		}
	}

	// Check if current model is a subscription model (cost is 0 for both input and output)
	isSubscriptionModel := m.app.Model != nil &&
		m.app.Model.Cost.Input == 0 && m.app.Model.Cost.Output == 0

	sessionInfo = styles.NewStyle().
		Foreground(t.TextMuted()).
		Background(t.Background()).
		Render(formatTokensAndCost(tokens, contextWindow, cost, isSubscriptionModel))

	background := t.Background()
	share = layout.Render(
		layout.FlexOptions{
			Background: &background,
			Direction:  layout.Row,
			Justify:    layout.JustifySpaceBetween,
			Align:      layout.AlignStretch,
			Width:      width - 6,
		},
		layout.FlexItem{
			View: share,
		},
		layout.FlexItem{
			View: sessionInfo,
		},
	)

	headerLines = append(headerLines, share)

	header := strings.Join(headerLines, "\n")

	header = styles.NewStyle().
		Background(t.Background()).
		Width(width).
		PaddingLeft(2).
		PaddingRight(2).
		BorderLeft(true).
		BorderRight(true).
		BorderBackground(t.Background()).
		BorderForeground(t.BackgroundElement()).
		BorderStyle(lipgloss.ThickBorder()).
		Render(header)

	return "\n" + header + "\n"
}

func formatTokensAndCost(
	tokens float64,
	contextWindow float64,
	cost float64,
	isSubscriptionModel bool,
) string {
	// Format tokens in human-readable format (e.g., 110K, 1.2M)
	var formattedTokens string
	switch {
	case tokens >= 1_000_000:
		formattedTokens = fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	case tokens >= 1_000:
		formattedTokens = fmt.Sprintf("%.1fK", float64(tokens)/1_000)
	default:
		formattedTokens = fmt.Sprintf("%d", int(tokens))
	}

	// Remove .0 suffix if present
	if strings.HasSuffix(formattedTokens, ".0K") {
		formattedTokens = strings.Replace(formattedTokens, ".0K", "K", 1)
	}
	if strings.HasSuffix(formattedTokens, ".0M") {
		formattedTokens = strings.Replace(formattedTokens, ".0M", "M", 1)
	}

	percentage := (float64(tokens) / float64(contextWindow)) * 100

	if isSubscriptionModel {
		return fmt.Sprintf(
			"%s/%d%%",
			formattedTokens,
			int(percentage),
		)
	}

	formattedCost := fmt.Sprintf("$%.2f", cost)
	return fmt.Sprintf(
		"%s/%d%% (%s)",
		formattedTokens,
		int(percentage),
		formattedCost,
	)
}

func (m *messagesComponent) View(width, height int) string {
	t := theme.CurrentTheme()
	if m.rendering {
		return lipgloss.Place(
			width,
			height,
			lipgloss.Center,
			lipgloss.Center,
			styles.NewStyle().Background(t.Background()).Render(""),
			styles.WhitespaceStyle(t.Background()),
		)
	}
	header := m.header(width)
	m.viewport.SetWidth(width)
	m.viewport.SetHeight(height - lipgloss.Height(header))

	return styles.NewStyle().
		Background(t.Background()).
		Render(header + "\n" + m.viewport.View())
}

func (m *messagesComponent) SetWidth(width int) tea.Cmd {
	if m.width == width {
		return nil
	}
	
	// Only invalidate cache if width changed significantly (more than 5 pixels)
	if abs(m.width-width) > 5 {
		m.invalidateMessageInfos()
	}
	
	m.width = width
	m.viewport.SetWidth(width)
	m.renderView(width)
	return nil
}

func (m *messagesComponent) Reload() tea.Cmd {
	return func() tea.Msg {
		m.renderView(m.width)
		return renderFinishedMsg{}
	}
}

func (m *messagesComponent) PageUp() (tea.Model, tea.Cmd) {
	m.viewport.ViewUp()
	return m, nil
}

func (m *messagesComponent) PageDown() (tea.Model, tea.Cmd) {
	m.viewport.ViewDown()
	return m, nil
}

func (m *messagesComponent) HalfPageUp() (tea.Model, tea.Cmd) {
	m.viewport.HalfViewUp()
	return m, nil
}

func (m *messagesComponent) HalfPageDown() (tea.Model, tea.Cmd) {
	m.viewport.HalfViewDown()
	return m, nil
}

func (m *messagesComponent) Previous() (tea.Model, tea.Cmd) {
	m.tail = false
	if m.selectedPart < 0 {
		m.selectedPart = m.partCount
	}
	m.selectedPart--
	if m.selectedPart < 0 {
		m.selectedPart = 0
	}
	return m, util.CmdHandler(selectedMessagePartChangedMsg{
		part: m.selectedPart,
	})
}

func (m *messagesComponent) Next() (tea.Model, tea.Cmd) {
	m.tail = false
	m.selectedPart++
	if m.selectedPart >= m.partCount {
		m.selectedPart = m.partCount
	}
	return m, util.CmdHandler(selectedMessagePartChangedMsg{
		part: m.selectedPart,
	})
}

func (m *messagesComponent) First() (tea.Model, tea.Cmd) {
	m.selectedPart = 0
	m.tail = false
	return m, util.CmdHandler(selectedMessagePartChangedMsg{
		part: m.selectedPart,
	})
}

func (m *messagesComponent) Last() (tea.Model, tea.Cmd) {
	m.selectedPart = m.partCount - 1
	m.tail = true
	return m, util.CmdHandler(selectedMessagePartChangedMsg{
		part: m.selectedPart,
	})
}

func (m *messagesComponent) ToolDetailsVisible() bool {
	return m.showToolDetails
}

func NewMessagesComponent(app *app.App) MessagesComponent {
	vp := viewport.New()
	vp.KeyMap = viewport.KeyMap{}

	return &messagesComponent{
		app:               app,
		viewport:          vp,
		showToolDetails:   true,
		cache:             NewMessageCache(),
		tail:              true,
		selectedPart:      -1,
		bufferSize:        3, // Render 3 extra messages above/below viewport
		messageInfos:      make([]MessageInfo, 0),
		renderThrottle:    16 * time.Millisecond, // ~60 FPS
		orphanedToolCalls: make([]opencode.ToolPart, 0),
	}
}

// Helper functions
func abs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// processOrphanedToolCalls processes all messages to collect orphaned tool calls
// This maintains the interim output (planning, tool usage, thoughts) logic
func (m *messagesComponent) processOrphanedToolCalls() {
	m.orphanedToolCalls = make([]opencode.ToolPart, 0)
	
	for _, message := range m.app.Messages {
		switch casted := message.(type) {
		case opencode.AssistantMessage:
			hasTextPart := false
			for _, p := range casted.Parts {
				switch part := p.AsUnion().(type) {
				case opencode.TextPart:
					hasTextPart = true
					// Clear orphaned tool calls when we hit a text part
					// (they'll be attached to this assistant message)
					m.orphanedToolCalls = make([]opencode.ToolPart, 0)
				case opencode.ToolPart:
					if !hasTextPart {
						// Tool calls without preceding text parts are "orphaned"
						// These represent interim AI activity (planning, tool usage, etc.)
						m.orphanedToolCalls = append(m.orphanedToolCalls, part)
					}
				}
			}
		}
	}
}

// getOrphanedToolCallsForMessage returns orphaned tool calls that should be
// included with the specified message (by index)
func (m *messagesComponent) getOrphanedToolCallsForMessage(messageIndex int) []opencode.ToolPart {
	if messageIndex >= len(m.app.Messages) {
		return nil
	}
	
	message := m.app.Messages[messageIndex]
	assistant, ok := message.(opencode.AssistantMessage)
	if !ok {
		return nil
	}
	
	// Check if this assistant message has a text part
	hasTextPart := false
	for _, p := range assistant.Parts {
		if _, ok := p.AsUnion().(opencode.TextPart); ok {
			hasTextPart = true
			break
		}
	}
	
	// Only return orphaned tool calls for assistant messages with text parts
	// (this is where the interim output gets attached)
	if hasTextPart {
		// Find orphaned tool calls that occurred before this message
		orphanedForThis := make([]opencode.ToolPart, 0)
		
		// Look at messages before this one to collect orphaned tool calls
		for i := messageIndex - 1; i >= 0; i-- {
			prevMessage := m.app.Messages[i]
			if prevAssistant, ok := prevMessage.(opencode.AssistantMessage); ok {
				// Check if previous assistant message had text parts
				prevHasTextPart := false
				for _, p := range prevAssistant.Parts {
					if _, ok := p.AsUnion().(opencode.TextPart); ok {
						prevHasTextPart = true
						break
					}
				}
				
				// If previous message had text, stop looking backwards
				if prevHasTextPart {
					break
				}
				
				// Collect tool calls from messages without text parts
				for _, p := range prevAssistant.Parts {
					if tool, ok := p.AsUnion().(opencode.ToolPart); ok {
						orphanedForThis = append([]opencode.ToolPart{tool}, orphanedForThis...)
					}
				}
			}
		}
		
		return orphanedForThis
	}
	
	return nil
}
