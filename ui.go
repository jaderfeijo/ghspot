package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/mattn/go-runewidth"
)

// The initial load pulls open and closed tickets in parallel, newest first.
// GitHub search caps each query at 1000 results; older tickets are still
// reachable through the remote title search and number lookup.
var stageQualifiers = []string{"is:open", "is:closed"}

const (
	firstPageSize = 30 // small, so the first screen paints fast
	pageSize      = 100
	remoteLimit   = 30
)

type kindFilter int

const (
	kindAll kindFilter = iota
	kindIssue
	kindPR
)

func (k kindFilter) String() string { return [...]string{"All", "Issues", "PRs"}[k] }

type stateFilter int

const (
	stateOpen stateFilter = iota
	stateClosed
	stateAll
)

func (s stateFilter) String() string { return [...]string{"Open", "Closed", "Any state"}[s] }

type (
	pageMsg struct {
		gen, stage int
		res        searchResult
		err        error
	}
	remoteMsg struct {
		items []*Item
		err   error
	}
	detailMsg struct {
		key      string
		item     *Item
		comments []Comment
		err      error
	}
	searchTick struct{ seq int }
	detailTick struct{ key string }
	flashClear struct{ seq int }
)

type model struct {
	client     *api.GraphQLClient
	sc         scope
	quitOnOpen bool
	icons      bool

	input   textinput.Model
	vp      viewport.Model
	spin    spinner.Model
	spinOn  bool
	w, h    int
	sized   bool
	preview bool
	reading bool // full-screen reader for the selected ticket

	items   []*Item
	byKey   map[string]*Item
	repos   []string // distinct repos among items, sorted
	results []result
	cursor  int
	offset  int
	moved   bool // user navigated since the last query change
	kind    kindFilter
	state   stateFilter

	gen        int
	loading    int // stages still paging
	searchSeq  int
	searching  int // in-flight remote lookups
	lastRemote string

	comments    map[string][]Comment
	detailBusy  map[string]bool
	previewKey  string
	renderer    *glamour.TermRenderer
	rendererW   int
	glamStyle   ansi.StyleConfig
	renderCache map[string]string

	err      string
	flash    string
	flashSeq int
}

func newModel(c *api.GraphQLClient, sc scope, query string, quitOnOpen, icons, dark bool) *model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = "Search by title or #number…"
	ti.SetValue(query)
	ti.Focus()

	st := styles.DarkStyleConfig
	if !dark {
		st = styles.LightStyleConfig
	}
	var zero uint
	st.Document.Margin = &zero
	st.Document.BlockPrefix = ""
	st.Document.BlockSuffix = ""

	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot), spinner.WithStyle(lipgloss.NewStyle().Foreground(colAccent)))

	return &model{
		client:      c,
		sc:          sc,
		quitOnOpen:  quitOnOpen,
		icons:       icons,
		input:       ti,
		vp:          viewport.New(0, 0),
		spin:        sp,
		preview:     true,
		byKey:       map[string]*Item{},
		comments:    map[string][]Comment{},
		detailBusy:  map[string]bool{},
		renderCache: map[string]string{},
		glamStyle:   st,
	}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.reload())
}

// ---------- commands ----------

func (m *model) reload() tea.Cmd {
	m.gen++
	m.loading = len(stageQualifiers)
	m.err = ""
	cmds := []tea.Cmd{m.startSpin()}
	for stage := range stageQualifiers {
		cmds = append(cmds, m.loadPage(m.gen, stage, ""))
	}
	return tea.Batch(cmds...)
}

func (m *model) loadPage(gen, stage int, after string) tea.Cmd {
	c := m.client
	q := m.sc.base + " " + stageQualifiers[stage] + " sort:updated-desc"
	first := pageSize
	if after == "" {
		first = firstPageSize
	}
	return func() tea.Msg {
		res, err := searchPage(c, q, after, first)
		return pageMsg{gen: gen, stage: stage, res: res, err: err}
	}
}

func (m *model) remoteSearch(q string, repos []string) tea.Cmd {
	c := m.client
	full := m.sc.base + " in:title " + q
	if len(repos) > 0 {
		// GitHub ORs repo: with org:/repo: qualifiers, so a named repo
		// replaces the scope instead of being added to it.
		full = "in:title " + q
		for _, r := range repos {
			full += " repo:" + r
		}
	}
	switch m.kind {
	case kindIssue:
		full += " is:issue"
	case kindPR:
		full += " is:pr"
	}
	switch m.state {
	case stateOpen:
		full += " is:open"
	case stateClosed:
		full += " is:closed"
	}
	m.searching++
	return tea.Batch(m.startSpin(), func() tea.Msg {
		res, err := searchPage(c, full, "", remoteLimit)
		return remoteMsg{items: res.items, err: err}
	})
}

func (m *model) lookupNumber(repo string, n int) tea.Cmd {
	c := m.client
	m.searching++
	return tea.Batch(m.startSpin(), func() tea.Msg {
		it, comments, err := fetchDetail(c, repo, n)
		if it == nil {
			return remoteMsg{err: err}
		}
		return detailMsg{key: it.Key(), item: it, comments: comments}
	})
}

func (m *model) loadDetail(it *Item) tea.Cmd {
	c, repo, n, key := m.client, it.Repo, it.Number, it.Key()
	m.detailBusy[key] = true
	return func() tea.Msg {
		item, comments, err := fetchDetail(c, repo, n)
		return detailMsg{key: key, item: item, comments: comments, err: err}
	}
}

func (m *model) startSpin() tea.Cmd {
	if m.spinOn {
		return nil
	}
	m.spinOn = true
	return m.spin.Tick
}

func (m *model) busy() bool { return m.loading > 0 || m.searching > 0 }

func (m *model) setFlash(s string) tea.Cmd {
	m.flash = s
	m.flashSeq++
	seq := m.flashSeq
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return flashClear{seq} })
}

// ---------- data ----------

func (m *model) merge(items []*Item) {
	for _, it := range items {
		if old, ok := m.byKey[it.Key()]; ok {
			*old = *it
			continue
		}
		m.byKey[it.Key()] = it
		m.items = append(m.items, it)
		if i := sort.SearchStrings(m.repos, it.Repo); i == len(m.repos) || m.repos[i] != it.Repo {
			m.repos = slices.Insert(m.repos, i, it.Repo)
		}
	}
	sort.SliceStable(m.items, func(a, b int) bool { return m.items[a].UpdatedAt.After(m.items[b].UpdatedAt) })
}

func (m *model) keep(it *Item) bool {
	switch m.kind {
	case kindIssue:
		if it.IsPR {
			return false
		}
	case kindPR:
		if !it.IsPR {
			return false
		}
	}
	switch m.state {
	case stateOpen:
		return it.State == "OPEN"
	case stateClosed:
		return it.State != "OPEN"
	}
	return true
}

// refilter recomputes results. With keepSel the current selection survives
// data arriving, but only once the user has navigated; until then the top
// result stays selected. A query or filter change always selects the top.
func (m *model) refilter(keepSel bool) tea.Cmd {
	sel := m.selected()
	m.results = filterItems(m.items, m.input.Value(), m.repos, m.keep)
	m.cursor = 0
	if !keepSel {
		m.moved = false
	}
	if keepSel && m.moved && sel != nil {
		for i, r := range m.results {
			if r.it == sel {
				m.cursor = i
				break
			}
		}
	}
	if !keepSel {
		m.offset = 0
	}
	return m.selectionChanged()
}

func (m *model) selected() *Item {
	if m.cursor >= 0 && m.cursor < len(m.results) {
		return m.results[m.cursor].it
	}
	return nil
}

func (m *model) move(d int) tea.Cmd {
	if len(m.results) == 0 {
		return nil
	}
	m.cursor = max(0, min(len(m.results)-1, m.cursor+d))
	m.moved = true
	return m.selectionChanged()
}

func (m *model) selectionChanged() tea.Cmd {
	_, _, bodyH := m.layout()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if bodyH > 0 && m.cursor >= m.offset+bodyH {
		m.offset = m.cursor - bodyH + 1
	}
	it := m.selected()
	key := ""
	if it != nil {
		key = it.Key()
	}
	reset := key != m.previewKey
	m.previewKey = key
	m.updatePreview(reset)
	if it == nil || !reset {
		return nil
	}
	if _, ok := m.comments[key]; ok || m.detailBusy[key] {
		return nil
	}
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return detailTick{key} })
}

// ---------- update ----------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		if !m.sized {
			m.sized = true
			m.preview = m.w >= 90
		}
		m.resize()
		return m, m.selectionChanged()

	case spinner.TickMsg:
		if !m.busy() {
			m.spinOn = false
			return m, nil
		}
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case flashClear:
		if msg.seq == m.flashSeq {
			m.flash = ""
		}
		return m, nil

	case pageMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		if msg.err != nil {
			m.loading--
			m.err = msg.err.Error()
			return m, nil
		}
		m.merge(msg.res.items)
		cmd := m.refilter(true)
		if msg.res.more {
			return m, tea.Batch(cmd, m.loadPage(msg.gen, msg.stage, msg.res.cursor))
		}
		m.loading--
		return m, cmd

	case remoteMsg:
		m.searching--
		if msg.err != nil {
			m.err = msg.err.Error()
		}
		if len(msg.items) == 0 {
			return m, nil
		}
		m.merge(msg.items)
		return m, m.refilter(true)

	case detailMsg:
		if m.detailBusy[msg.key] {
			delete(m.detailBusy, msg.key)
		} else {
			m.searching-- // came from a number lookup
		}
		if msg.err != nil {
			m.err = msg.err.Error()
			return m, nil
		}
		if msg.item == nil {
			return m, nil
		}
		m.comments[msg.key] = msg.comments
		m.merge([]*Item{msg.item})
		return m, m.refilter(true)

	case searchTick:
		if msg.seq != m.searchSeq {
			return m, nil
		}
		scoped, q := parseQuery(m.input.Value(), m.repos)
		if n, ok := numberQuery(q); ok {
			repo := m.sc.repo
			if len(scoped) == 1 {
				repo = scoped[0]
			}
			if repo == "" || m.byKey[fmt.Sprintf("%s#%d", repo, n)] != nil {
				return m, nil
			}
			return m, m.lookupNumber(repo, n)
		}
		remoteKey := fmt.Sprintf("%s|%v|%d|%d", q, scoped, m.kind, m.state)
		if len([]rune(q)) < 2 || remoteKey == m.lastRemote {
			return m, nil
		}
		m.lastRemote = remoteKey
		return m, m.remoteSearch(q, scoped)

	case detailTick:
		it := m.selected()
		if it == nil || it.Key() != msg.key || m.detailBusy[msg.key] {
			return m, nil
		}
		if _, ok := m.comments[msg.key]; ok {
			return m, nil
		}
		return m, m.loadDetail(it)

	case tea.MouseMsg:
		return m, m.handleMouse(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *model) openSelected(quit bool) tea.Cmd {
	it := m.selected()
	if it == nil {
		return nil
	}
	if err := openURL(it.URL); err != nil {
		m.err = "open: " + err.Error()
		return nil
	}
	if quit || m.quitOnOpen {
		return tea.Quit
	}
	return m.setFlash(fmt.Sprintf("Opened #%d in browser", it.Number))
}

func (m *model) copySelected() tea.Cmd {
	it := m.selected()
	if it == nil {
		return nil
	}
	if err := copyText(it.URL); err != nil {
		m.err = "copy: " + err.Error()
		return nil
	}
	return m.setFlash("Copied " + it.URL)
}

func (m *model) setReading(on bool) tea.Cmd {
	m.reading = on
	m.resize()
	m.previewKey = "" // force a re-render at the new width, from the top
	return m.selectionChanged()
}

func (m *model) handleReaderKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc", "q", "backspace", "left", "h":
		return m, m.setReading(false)
	case "o", "enter":
		return m, m.openSelected(false)
	case "O":
		return m, m.openSelected(true)
	case "y", "ctrl+y":
		return m, m.copySelected()
	case "n", "ctrl+n":
		return m, m.move(1)
	case "p", "N", "ctrl+p":
		return m, m.move(-1)
	case "j", "down":
		m.vp.ScrollDown(1)
	case "k", "up":
		m.vp.ScrollUp(1)
	case "d", "ctrl+d":
		m.vp.HalfPageDown()
	case "u", "ctrl+u":
		m.vp.HalfPageUp()
	case " ", "f", "pgdown":
		m.vp.PageDown()
	case "b", "pgup":
		m.vp.PageUp()
	case "g", "home":
		m.vp.GotoTop()
	case "G", "end":
		m.vp.GotoBottom()
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.reading {
		return m.handleReaderKey(msg)
	}
	_, _, bodyH := m.layout()
	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit
	case "esc":
		if m.input.Value() != "" {
			m.input.SetValue("")
			return m, m.queryChanged()
		}
		return m, tea.Quit
	case "up", "ctrl+p":
		return m, m.move(-1)
	case "down", "ctrl+n":
		return m, m.move(1)
	case "ctrl+b":
		return m, m.move(-bodyH)
	case "ctrl+f":
		return m, m.move(bodyH)
	case "pgup":
		m.vp.HalfPageUp()
		return m, nil
	case "pgdown":
		m.vp.HalfPageDown()
		return m, nil
	case "shift+up":
		m.vp.ScrollUp(1)
		return m, nil
	case "shift+down":
		m.vp.ScrollDown(1)
		return m, nil
	case "enter":
		if m.selected() == nil {
			return m, nil
		}
		return m, m.setReading(true)
	case "ctrl+o":
		return m, m.openSelected(false)
	case "alt+enter":
		return m, m.openSelected(true)
	case "ctrl+y":
		return m, m.copySelected()
	case "tab":
		m.kind = (m.kind + 1) % 3
		return m, m.queryChanged()
	case "shift+tab", "ctrl+s":
		m.state = (m.state + 1) % 3
		return m, m.queryChanged()
	case "ctrl+t":
		m.preview = !m.preview
		m.resize()
		return m, m.selectionChanged()
	case "ctrl+r":
		m.items, m.byKey, m.repos, m.lastRemote = nil, map[string]*Item{}, nil, ""
		m.comments, m.renderCache = map[string][]Comment{}, map[string]string{}
		m.refilter(false)
		return m, tea.Batch(m.reload(), m.setFlash("Reloading…"))
	}
	old := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.input.Value() != old {
		return m, tea.Batch(cmd, m.queryChanged())
	}
	return m, cmd
}

func (m *model) queryChanged() tea.Cmd {
	m.searchSeq++
	seq := m.searchSeq
	return tea.Batch(m.refilter(false), tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return searchTick{seq} }))
}

func (m *model) handleMouse(msg tea.MouseMsg) tea.Cmd {
	listW, _, bodyH := m.layout()
	inList := msg.X < listW
	if msg.Action != tea.MouseActionPress {
		return nil
	}
	if m.reading {
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.vp.ScrollUp(3)
		case tea.MouseButtonWheelDown:
			m.vp.ScrollDown(3)
		}
		return nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if inList {
			return m.move(-3)
		}
		m.vp.ScrollUp(3)
	case tea.MouseButtonWheelDown:
		if inList {
			return m.move(3)
		}
		m.vp.ScrollDown(3)
	case tea.MouseButtonLeft:
		row := msg.Y - 2
		if inList && row >= 0 && row < bodyH && m.offset+row < len(m.results) {
			m.cursor = m.offset + row
			m.moved = true
			return m.selectionChanged()
		}
	}
	return nil
}

// ---------- layout ----------

const headerH, footerH = 2, 1

// layout returns the list width, preview width (including its border) and
// the height shared by both.
func (m *model) layout() (listW, prevW, bodyH int) {
	bodyH = max(0, m.h-headerH-footerH)
	if m.reading {
		return 0, m.w, bodyH
	}
	if !m.preview {
		return m.w, 0, bodyH
	}
	if m.w < 90 {
		return 0, m.w, bodyH
	}
	listW = max(44, m.w*2/5)
	return listW, m.w - listW, bodyH
}

func (m *model) resize() {
	_, prevW, bodyH := m.layout()
	m.vp.Width = max(0, prevW-2)
	if m.reading {
		m.vp.Width = min(max(0, m.w-4), 110) // comfortable line length
	}
	m.vp.Height = bodyH
	m.input.Width = max(10, m.w-30)
}

// ---------- preview ----------

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

func (m *model) markdown(s string, width int) string {
	s = strings.TrimSpace(htmlComment.ReplaceAllString(strings.ReplaceAll(s, "\r\n", "\n"), ""))
	if s == "" {
		return ""
	}
	if m.renderer == nil || m.rendererW != width {
		r, err := glamour.NewTermRenderer(glamour.WithStyles(m.glamStyle), glamour.WithWordWrap(width))
		if err != nil {
			return s
		}
		m.renderer, m.rendererW = r, width
	}
	out, err := m.renderer.Render(s)
	if err != nil {
		return s
	}
	return strings.Trim(out, "\n")
}

func (m *model) updatePreview(resetScroll bool) {
	it := m.selected()
	w := m.vp.Width
	if it == nil || w <= 0 {
		m.vp.SetContent("")
		return
	}
	comments, loaded := m.comments[it.Key()]
	ck := fmt.Sprintf("%s|%d|%d|%t", it.Key(), w, it.UpdatedAt.Unix(), loaded)
	content, ok := m.renderCache[ck]
	if !ok {
		content = m.renderPreview(it, comments, loaded, w)
		m.renderCache[ck] = content
	}
	m.vp.SetContent(content)
	if resetScroll {
		m.vp.GotoTop()
	}
}

func (m *model) renderPreview(it *Item, comments []Comment, loaded bool, w int) string {
	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Width(w).Render(it.Title))
	b.WriteString("\n\n")

	icon, col, word := m.stateBadge(it)
	badge := lipgloss.NewStyle().Foreground(colOnBadge).Background(col).Bold(true).Padding(0, 1).Render(icon + " " + word)
	meta := fmt.Sprintf("#%d · %s", it.Number, it.Repo)
	b.WriteString(badge + " " + dim.Render(meta) + "\n")
	b.WriteString(dim.Render(fmt.Sprintf("@%s opened %s · updated %s", it.Author, ago(it.CreatedAt), ago(it.UpdatedAt))) + "\n")

	if it.IsPR {
		line := fmt.Sprintf("%s ← %s  ", it.BaseRef, it.HeadRef)
		line = dim.Render(line) + lipgloss.NewStyle().Foreground(colGreen).Render(fmt.Sprintf("+%d", it.Additions)) +
			" " + lipgloss.NewStyle().Foreground(colRed).Render(fmt.Sprintf("−%d", it.Deletions))
		if it.ReviewDecision != "" {
			line += dim.Render(" · " + strings.ToLower(strings.ReplaceAll(it.ReviewDecision, "_", " ")))
		}
		b.WriteString(line + "\n")
	}
	if len(it.Assignees) > 0 {
		b.WriteString(dim.Render("assigned: @"+strings.Join(it.Assignees, ", @")) + "\n")
	}
	if len(it.Labels) > 0 {
		var chips []string
		for _, l := range it.Labels {
			chips = append(chips, labelChip(l))
		}
		b.WriteString(lipgloss.NewStyle().Width(w).Render(strings.Join(chips, " ")) + "\n")
	}
	b.WriteString(dim.Render(strings.Repeat("─", w)) + "\n\n")

	if body := m.markdown(it.Body, w); body != "" {
		b.WriteString(body)
	} else {
		b.WriteString(dim.Italic(true).Render("No description provided."))
	}
	b.WriteString("\n\n")

	switch {
	case loaded && len(comments) > 0:
		head := fmt.Sprintf(" %d comment%s ", it.Comments, plural(it.Comments))
		if it.Comments > len(comments) {
			head = fmt.Sprintf(" last %d of %d comments ", len(comments), it.Comments)
		}
		b.WriteString(dim.Render("──" + head + strings.Repeat("─", max(0, w-2-runewidth.StringWidth(head)))))
		for _, c := range comments {
			b.WriteString("\n\n" + lipgloss.NewStyle().Foreground(colAccent).Bold(true).Render("@"+c.Author) + dim.Render(" · "+ago(c.CreatedAt)) + "\n")
			b.WriteString(m.markdown(c.Body, w))
		}
	case !loaded && it.Comments > 0:
		b.WriteString(dim.Render(fmt.Sprintf("Loading %d comment%s…", it.Comments, plural(it.Comments))))
	}
	// Hard clip so a stray wide line can never break the pane layout.
	return lipgloss.NewStyle().MaxWidth(w).Render(b.String())
}

// ---------- view ----------

var (
	colAccent  = lipgloss.Color("4")
	colGreen   = lipgloss.Color("2")
	colRed     = lipgloss.Color("1")
	colPurple  = lipgloss.Color("5")
	colGray    = lipgloss.Color("8")
	colMatch   = lipgloss.Color("3")
	colOnBadge = lipgloss.Color("0")
	dim        = lipgloss.NewStyle().Foreground(colGray)
)

func (m *model) stateBadge(it *Item) (icon string, col lipgloss.Color, word string) {
	type g struct{ nerd, plain string }
	pick := func(x g) string {
		if m.icons {
			return x.nerd
		}
		return x.plain
	}
	switch {
	case it.IsPR && it.State == "MERGED":
		return pick(g{"", "M"}), colPurple, "Merged"
	case it.IsPR && it.State == "CLOSED":
		return pick(g{"", "x"}), colRed, "Closed"
	case it.IsPR && it.IsDraft:
		return pick(g{"", "d"}), colGray, "Draft"
	case it.IsPR:
		return pick(g{"", "P"}), colGreen, "Open"
	case it.State == "CLOSED" && it.StateReason == "NOT_PLANNED":
		return pick(g{"", "x"}), colGray, "Not planned"
	case it.State == "CLOSED":
		return pick(g{"", "✓"}), colPurple, "Closed"
	default:
		return pick(g{"", "o"}), colGreen, "Open"
	}
}

func (m *model) View() string {
	if !m.sized {
		return ""
	}
	if m.reading {
		return m.viewReader()
	}
	listW, prevW, bodyH := m.layout()

	// Line 1: prompt, query, filter chips.
	prompt := lipgloss.NewStyle().Foreground(colAccent).Bold(true).Render(" ❯ ")
	chip := func(s string, c lipgloss.Color) string {
		return lipgloss.NewStyle().Foreground(colOnBadge).Background(c).Padding(0, 1).Render(s)
	}
	stateCol := map[stateFilter]lipgloss.Color{stateOpen: colGreen, stateClosed: colPurple, stateAll: colGray}[m.state]
	chips := chip(m.kind.String(), colAccent) + " " + chip(m.state.String(), stateCol) + " "
	m.input.Width = max(5, m.w-lipgloss.Width(prompt)-lipgloss.Width(chips)-2)
	line1 := prompt + m.input.View()
	line1 += strings.Repeat(" ", max(1, m.w-lipgloss.Width(line1)-lipgloss.Width(chips))) + chips

	// Line 2: status rule.
	var info []string
	info = append(info, fmt.Sprintf("%d/%d", len(m.results), len(m.items)))
	if m.busy() {
		what := "loading"
		if m.loading == 0 {
			what = "searching GitHub"
		}
		info = append(info, m.spin.View()+" "+what)
	}
	if scoped, _ := parseQuery(m.input.Value(), m.repos); len(scoped) > 0 {
		names := strings.Join(scoped, ", ")
		info = append(info, lipgloss.NewStyle().Foreground(colAccent).Render("in "+runewidth.Truncate(names, 60, "…")))
	} else {
		info = append(info, m.sc.label)
	}
	status := " " + strings.Join(info, " · ") + " "
	switch {
	case m.err != "":
		status = lipgloss.NewStyle().Foreground(colRed).Render(" " + firstLine(m.err) + " ")
	case m.flash != "":
		status = lipgloss.NewStyle().Foreground(colGreen).Render(" " + m.flash + " ")
	}
	line2 := dim.Render("─") + status
	line2 = clip(line2, m.w)
	line2 += dim.Render(strings.Repeat("─", max(0, m.w-lipgloss.Width(line2))))

	// Body.
	var panes []string
	if listW > 0 {
		panes = append(panes, m.viewList(listW, bodyH))
	}
	if prevW > 0 {
		pv := lipgloss.NewStyle().Width(prevW - 2).Height(bodyH).MaxHeight(bodyH).Render(m.vp.View())
		if listW > 0 {
			pv = lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(colGray).PaddingLeft(1).Render(pv)
		} else {
			pv = lipgloss.NewStyle().PaddingLeft(1).PaddingRight(1).Render(pv)
		}
		panes = append(panes, pv)
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, panes...)

	help := []string{"↑↓ move", "⏎ read", "^o browser", "tab type", "^s state", "^y copy url", "pgup/pgdn scroll", "^t preview", "^r reload", "esc quit"}
	footer := clip(dim.Render(" "+strings.Join(help, " · ")), m.w)

	return lipgloss.JoinVertical(lipgloss.Left, line1, line2, body, footer)
}

func (m *model) viewReader() string {
	_, _, bodyH := m.layout()
	it := m.selected()
	top := ""
	if it != nil {
		icon, col, _ := m.stateBadge(it)
		top = " " + lipgloss.NewStyle().Foreground(col).Render(icon) + " " +
			dim.Render(fmt.Sprintf("%s#%d", it.Repo, it.Number)) + "  " +
			lipgloss.NewStyle().Bold(true).Render(it.Title)
	}
	pos := fmt.Sprintf(" %d/%d · %3.f%% ", m.cursor+1, len(m.results), m.vp.ScrollPercent()*100)
	switch {
	case m.err != "":
		pos = lipgloss.NewStyle().Foreground(colRed).Render(" "+firstLine(m.err)+" ") + pos
	case m.flash != "":
		pos = lipgloss.NewStyle().Foreground(colGreen).Render(" "+m.flash+" ") + pos
	}
	top = clip(top, max(0, m.w-lipgloss.Width(pos)-1))
	top += strings.Repeat(" ", max(1, m.w-lipgloss.Width(top)-lipgloss.Width(pos))) + dim.Render(pos)
	rule := dim.Render(strings.Repeat("─", m.w))

	pad := max(2, (m.w-m.vp.Width)/2)
	body := lipgloss.NewStyle().PaddingLeft(pad).Height(bodyH).MaxHeight(bodyH).Render(m.vp.View())

	help := []string{"esc back", "o open in browser", "O open & quit", "y copy url", "j/k scroll", "space/b page", "g/G top/bottom", "n/p next/prev"}
	footer := clip(dim.Render(" "+strings.Join(help, " · ")), m.w)
	return lipgloss.JoinVertical(lipgloss.Left, top, rule, body, footer)
}

func (m *model) viewList(w, h int) string {
	lines := make([]string, 0, h)
	if len(m.results) == 0 {
		msg := "No matches"
		if m.loading > 0 && len(m.items) == 0 {
			msg = "Loading…"
		} else if m.searching > 0 {
			msg = "Searching GitHub…"
		}
		lines = append(lines, dim.Render("  "+msg))
	}
	numW := 0
	for _, r := range m.results {
		numW = max(numW, len(fmt.Sprint(r.it.Number))+1)
	}
	crossRepo := m.sc.repo == ""
	for i := m.offset; i < len(m.results) && len(lines) < h; i++ {
		lines = append(lines, m.viewRow(m.results[i], i == m.cursor, w, numW, crossRepo))
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return lipgloss.NewStyle().Width(w).Render(strings.Join(lines, "\n"))
}

func (m *model) viewRow(r result, sel bool, w, numW int, crossRepo bool) string {
	it := r.it
	bar := " "
	if sel {
		bar = lipgloss.NewStyle().Foreground(colAccent).Render("▌")
	}
	icon, col, _ := m.stateBadge(it)
	num := fmt.Sprintf("%*s", numW, fmt.Sprintf("#%d", it.Number))
	age := fmt.Sprintf("%4s", shortAgo(it.UpdatedAt))
	prefix := bar + lipgloss.NewStyle().Foreground(col).Render(icon) + " " + dim.Render(num) + " "
	if crossRepo {
		repo := it.Repo[strings.Index(it.Repo, "/")+1:]
		rs := dim
		if r.repoHit {
			rs = lipgloss.NewStyle().Foreground(colAccent)
		}
		prefix += rs.Render(runewidth.Truncate(repo, 18, "…")) + " "
	}
	titleW := w - lipgloss.Width(prefix) - lipgloss.Width(age) - 2

	base := lipgloss.NewStyle()
	if sel {
		base = base.Bold(true)
	}
	hl := base.Foreground(colMatch).Underline(true)
	var tb strings.Builder
	used := 0
	truncated := runewidth.StringWidth(it.Title) > titleW
	for i, ch := range it.Title {
		cw := runewidth.RuneWidth(ch)
		if truncated && used+cw > titleW-1 {
			tb.WriteString(base.Render("…"))
			used++
			break
		}
		if r.matched[i] {
			tb.WriteString(hl.Render(string(ch)))
		} else {
			tb.WriteString(base.Render(string(ch)))
		}
		used += cw
	}
	pad := strings.Repeat(" ", max(1, titleW-used+1))
	return prefix + tb.String() + pad + dim.Render(age)
}

// ---------- helpers ----------

func labelChip(l Label) string {
	bg := lipgloss.Color("#" + l.Color)
	fg := lipgloss.Color("#ffffff")
	var r, g, b int
	if _, err := fmt.Sscanf(l.Color, "%02x%02x%02x", &r, &g, &b); err == nil && (r*299+g*587+b*114)/1000 > 140 {
		fg = lipgloss.Color("#000000")
	}
	return lipgloss.NewStyle().Foreground(fg).Background(bg).Padding(0, 1).Render(l.Name)
}

func clip(s string, w int) string { return lipgloss.NewStyle().MaxWidth(w).Render(s) }

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func shortAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", max(1, int(d.Minutes())))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo", int(d.Hours()/24/30))
	}
	return fmt.Sprintf("%dy", int(d.Hours()/24/365))
}

func ago(t time.Time) string {
	if time.Since(t) > 365*24*time.Hour {
		return t.Format("Jan 2, 2006")
	}
	return shortAgo(t) + " ago"
}

func openURL(u string) error {
	name, args := "xdg-open", []string{u}
	if b := os.Getenv("BROWSER"); b != "" {
		f := strings.Fields(b)
		name, args = f[0], append(f[1:], u)
	}
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func copyText(s string) error {
	name, args := "wl-copy", []string{}
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		name, args = "xclip", []string{"-selection", "clipboard"}
	}
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}
