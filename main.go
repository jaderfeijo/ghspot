package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/repository"
)

// scope is the slice of GitHub being searched, expressed as a search qualifier.
type scope struct {
	base  string // e.g. "repo:owner/name", "org:acme", "involves:@me"
	label string
	repo  string // set only when scoped to a single repo
}

func resolveScope(repoFlag, org string, mine bool) (scope, error) {
	switch {
	case repoFlag != "":
		repoFlag = strings.TrimPrefix(strings.TrimSuffix(repoFlag, ".git"), "https://github.com/")
		if strings.Count(repoFlag, "/") != 1 {
			return scope{}, fmt.Errorf("-R expects OWNER/REPO, got %q", repoFlag)
		}
		return scope{base: "repo:" + repoFlag, label: repoFlag, repo: repoFlag}, nil
	case org != "":
		return scope{base: "org:" + org, label: "org:" + org}, nil
	case mine:
		return scope{base: "involves:@me", label: "involving you"}, nil
	}
	if r, err := repository.Current(); err == nil && r.Host == "github.com" {
		full := r.Owner + "/" + r.Name
		return scope{base: "repo:" + full, label: full, repo: full}, nil
	}
	return scope{base: "involves:@me", label: "involving you"}, nil
}

func main() {
	repoFlag := flag.String("R", "", "search `OWNER/REPO` (default: the current git repo)")
	org := flag.String("org", "", "search every repo in an `ORG`")
	mine := flag.Bool("mine", false, "search issues and PRs involving you across GitHub")
	once := flag.Bool("1", false, "quit after opening a ticket in the browser")
	noIcons := flag.Bool("no-icons", false, "use plain glyphs instead of Nerd Font icons")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "ghspot — spotlight search for GitHub issues and pull requests\n\n")
		fmt.Fprintf(os.Stderr, "usage: ghspot [flags] [initial query]\n\n")
		fmt.Fprintf(os.Stderr, "Outside a GitHub repo with no flags, searches everything involving you.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	sc, err := resolveScope(*repoFlag, *org, *mine)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghspot:", err)
		os.Exit(2)
	}
	client, err := api.NewGraphQLClient(api.ClientOptions{Timeout: 30 * time.Second})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ghspot:", err, "\n(run `gh auth login` first)")
		os.Exit(1)
	}

	dark := lipgloss.HasDarkBackground()
	m := newModel(client, sc, strings.Join(flag.Args(), " "), *once, !*noIcons, dark)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "ghspot:", err)
		os.Exit(1)
	}
}
