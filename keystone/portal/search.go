package portal

// The household's web search.
//
// The front page's search box used to filter the app launcher, and its
// results appeared three cards further down the page — so the one thing
// a member types into answered somewhere they were not looking. §7's
// objection to a web search on this surface is specific and worth
// quoting, because this is deliberately not it: "it does not offer a
// web search — this box searches the household, and SILENTLY becoming
// an internet search box would be a privacy surprise." The failure is
// the silence. A box that names the engine it will use, next to a
// separate box that says it searches apps, is the opposite.
//
// A CLOSED TABLE, NOT A URL FROM CONFIGURATION. The engine is chosen by
// name and the address comes from here. A settings file that could
// supply the URL would make a typo into a redirect to anywhere and a
// hostile edit into a credential-phishing page wearing the household's
// own dashboard as a referrer.
//
// NOTHING LEAVES THE HOUSE UNTIL SOMEBODY PRESSES ENTER. There is no
// suggestion endpoint and no as-you-type request: a dashboard that sent
// every keystroke to a search engine would be handing over more than
// the searches, and on a wall tablet it would do it for whoever walked
// past.

import "strings"

// Engine is one search engine a household can choose.
type Engine struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	// Query is the address a search goes to, with the terms appended.
	// Percent-encoded by the caller; this is the prefix only.
	Query string `json:"query"`
}

// Engines is the closed set. DuckDuckGo first because it is the default
// and the default should be the one that asks the household for least.
var Engines = []Engine{
	{Key: "duckduckgo", Name: "DuckDuckGo", Query: "https://duckduckgo.com/?q="},
	{Key: "google", Name: "Google", Query: "https://www.google.com/search?q="},
	{Key: "bing", Name: "Bing", Query: "https://www.bing.com/search?q="},
	{Key: "yahoo", Name: "Yahoo", Query: "https://search.yahoo.com/search?p="},
	{Key: "brave", Name: "Brave", Query: "https://search.brave.com/search?q="},
	{Key: "startpage", Name: "Startpage", Query: "https://www.startpage.com/sp/search?query="},
	{Key: "ecosia", Name: "Ecosia", Query: "https://www.ecosia.org/search?q="},
	{Key: "wikipedia", Name: "Wikipedia", Query: "https://en.wikipedia.org/w/index.php?search="},
}

// DefaultEngine is what a household that has not chosen gets.
//
// DuckDuckGo rather than Google, and that is a decision rather than a
// coin toss: this box sits on a family's front page, a household that
// never opens the settings still gets one, and the one they get should
// not be the one that keeps the most.
const DefaultEngine = "duckduckgo"

// EngineFor resolves a configured key to its row, falling back to the
// default. The bool reports whether the key was RECOGNISED — false for
// both "nothing was set" and "something unusable was set".
//
// A key nobody recognises falls back rather than failing, because the
// alternative is a household whose search box stops working because of
// a typo in a settings file. The caller can still tell the difference
// and say so; it just is not the search box's job to break over it.
func EngineFor(key string) (Engine, bool) {
	want := strings.ToLower(strings.TrimSpace(key))
	for _, e := range Engines {
		if e.Key == want {
			return e, true
		}
	}
	for _, e := range Engines {
		if e.Key == DefaultEngine {
			return e, false
		}
	}
	return Engine{}, false
}
