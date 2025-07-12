# OpenCode TUI Performance Analysis Report

## Project Overview

OpenCode is a sophisticated AI development environment with a TUI built using Go and the Bubble Tea framework. The architecture consists of:

- **Backend**: Node.js/Bun server handling AI interactions
- **Frontend**: Go-based TUI using Bubble Tea framework  
- **Communication**: HTTP-based API with event streaming
- **Key Components**: Messages, editor, file viewer, completions, status bar

## Architecture Analysis

### TUI Structure (`packages/tui/`)
```
internal/
├── app/           # Application state management
├── components/    # UI components (chat, editor, fileviewer, etc.)
├── tui/          # Main TUI orchestration
├── layout/       # Layout management
├── styles/       # Styling with lipgloss
└── util/         # Utilities including performance measurement
```

### Entry Point Flow
1. `cmd/opencode/main.go` - Creates Bubble Tea program
2. `internal/tui/tui.go` - Main UI model and update logic
3. `internal/components/chat/messages.go` - Chat message rendering
4. Event streaming from backend drives real-time updates

## Root Cause Analysis

### Primary Issue: Quadratic Performance Degradation

The core performance problem is in `internal/components/chat/messages.go`:

```go
func (m *messagesComponent) renderView(width int) {
    measure := util.Measure("messages.renderView")
    defer measure("messageCount", len(m.app.Messages))
    
    // PROBLEM: Processes ALL messages on every update
    for _, message := range m.app.Messages {
        // Complex rendering logic for each message
        // This includes styling, layout, syntax highlighting
    }
}
```

**Impact**: As conversation length grows, render time increases linearly with message count, but this function is called frequently, creating O(n²) overall complexity.

### Secondary Issues

#### 1. Excessive Cache Invalidation
The message cache (`cache.go`) gets cleared too frequently:
- Width changes clear entire cache
- Theme changes clear entire cache  
- Tool details toggle clears entire cache
- Session changes clear entire cache

#### 2. No Virtualization
The viewport renders all messages even if not visible:
- No virtual scrolling implementation
- All messages stay in memory and get processed
- Viewport scrolling still processes full content

#### 3. High-Frequency Updates
Continuous event streaming triggers frequent re-renders:
```go
// main.go - Continuous event stream
go func() {
    stream := httpClient.Event.ListStreaming(ctx)
    for stream.Next() {
        evt := stream.Current().AsUnion()
        program.Send(evt) // Triggers UI update
    }
}()
```

#### 4. Synchronous Rendering
All rendering happens on the main thread with no batching or throttling.

## Performance Symptoms Explained

- **Text rendering slowdown**: `renderView()` takes longer as message count grows
- **Input lag**: Frequent re-renders block the UI thread
- **High CPU usage**: Continuous processing of growing message history
- **Backspace lag**: Input processing gets queued behind rendering operations

## Recommended Solutions

### Immediate High-Impact Fixes

#### 1. Implement Message Virtualization ⭐⭐⭐
**Impact**: Changes O(n) to O(viewport_size) rendering

```go
// Pseudo-code for virtual scrolling
type VirtualizedMessages struct {
    viewport       viewport.Model
    messageHeights []int
    visibleStart   int
    visibleEnd     int
    totalHeight    int
}

func (v *VirtualizedMessages) renderVisible() {
    // Only render messages currently in viewport
    for i := v.visibleStart; i <= v.visibleEnd; i++ {
        // Render only visible messages
    }
}
```

#### 2. Improve Cache Strategy ⭐⭐
**Impact**: Reduces redundant rendering work

- Use more granular cache keys (per message, not per view)
- Don't clear entire cache on width changes
- Implement LRU eviction instead of full clearing
- Cache individual message components separately

#### 3. Implement Render Batching/Throttling ⭐⭐
**Impact**: Reduces update frequency during streaming

```go
// Debounce rapid updates
type RenderScheduler struct {
    pending bool
    timer   *time.Timer
}

func (r *RenderScheduler) scheduleRender() {
    if r.pending { return }
    r.pending = true
    r.timer = time.AfterFunc(16*time.Millisecond, func() {
        r.pending = false
        // Trigger actual render
    })
}
```

### Secondary Optimizations

#### 4. Incremental Message Updates ⭐
- Track message change states
- Only re-render modified messages
- Use dirty flags for individual messages

#### 5. Memory Management
- Implement message history limits (e.g., keep last 1000 messages)
- Archive old messages to reduce active memory
- Lazy loading for message content

#### 6. Layout Optimization
- Cache expensive layout calculations
- Reduce lipgloss style object creation
- Pre-compute static styling

### Implementation Priority

1. **Week 1**: Message virtualization (biggest impact)
2. **Week 2**: Render batching/throttling
3. **Week 3**: Improved caching strategy
4. **Week 4**: Incremental updates and memory management

## Files to Modify

### Primary Changes
- `internal/components/chat/messages.go` - Implement virtualization
- `internal/components/chat/cache.go` - Improve cache strategy
- `internal/tui/tui.go` - Add render scheduling

### Supporting Changes
- `internal/app/app.go` - Message state management
- `internal/layout/layout.go` - Virtual layout helpers
- `internal/util/util.go` - Performance utilities

## Testing Strategy

1. **Performance benchmarks**: Measure render times with varying message counts
2. **Memory profiling**: Track memory usage over long sessions
3. **User testing**: Verify responsiveness improvements
4. **Load testing**: Test with very long conversations (1000+ messages)

## Expected Outcomes

- **90%+ reduction** in render times for long conversations
- **Consistent performance** regardless of conversation length  
- **Responsive input** even during heavy streaming
- **Lower memory usage** through better cache management

The virtualization fix alone should resolve the core performance issue, while the other optimizations will provide additional improvements and prevent regression.