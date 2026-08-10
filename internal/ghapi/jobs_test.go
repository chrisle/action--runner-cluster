package ghapi

import "testing"

func TestJobSelfHosted(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"explicit self-hosted", []string{"self-hosted", "linux"}, true},
		{"custom label alone", []string{"my-big-mac"}, true},
		{"self-hosted with custom label", []string{"self-hosted", "gpu"}, true},

		// These must not drive scaling: GitHub runs them on its own hosted
		// machines and would never route them to our runners, so treating them
		// as demand would spin up runners that sit idle until culled.
		{"github hosted ubuntu", []string{"ubuntu-latest"}, false},
		{"github hosted macos", []string{"macos-14"}, false},
		{"github hosted windows", []string{"windows-2022"}, false},
		{"github hosted bare os", []string{"ubuntu"}, false},

		{"no labels", nil, false},

		// A self-hosted runner labelled to shadow a hosted image is unusual but
		// legal; the explicit self-hosted label settles it.
		{"self-hosted shadowing a hosted label", []string{"self-hosted", "ubuntu-latest"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Job{Labels: tt.labels}.SelfHosted()
			if got != tt.want {
				t.Errorf("SelfHosted(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

func TestRepoFilterMatch(t *testing.T) {
	tests := []struct {
		name   string
		filter RepoFilterOpts
		repo   string
		want   bool
	}{
		{"no filter allows all", RepoFilterOpts{}, "anything", true},
		{"include exact", RepoFilterOpts{Include: []string{"api"}}, "api", true},
		{"include excludes others", RepoFilterOpts{Include: []string{"api"}}, "web", false},
		{"include glob", RepoFilterOpts{Include: []string{"service-*"}}, "service-auth", true},
		{"include glob miss", RepoFilterOpts{Include: []string{"service-*"}}, "web", false},
		{"exclude wins", RepoFilterOpts{Exclude: []string{"archive-*"}}, "archive-old", false},
		{"exclude leaves others", RepoFilterOpts{Exclude: []string{"archive-*"}}, "api", true},
		{"case insensitive", RepoFilterOpts{Include: []string{"API"}}, "api", true},
		{
			"exclude overrides include",
			RepoFilterOpts{Include: []string{"*"}, Exclude: []string{"secret"}},
			"secret",
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.filter.match(tt.repo); got != tt.want {
				t.Errorf("match(%q) = %v, want %v", tt.repo, got, tt.want)
			}
		})
	}
}

func TestNextLink(t *testing.T) {
	header := `<https://api.github.com/orgs/a/repos?page=2>; rel="next", ` +
		`<https://api.github.com/orgs/a/repos?page=9>; rel="last"`
	got := nextLink(header)
	want := "https://api.github.com/orgs/a/repos?page=2"
	if got != want {
		t.Errorf("nextLink = %q, want %q", got, want)
	}

	// The last page has no next link; returning something here would loop.
	if got := nextLink(`<https://api.github.com/orgs/a/repos?page=1>; rel="prev"`); got != "" {
		t.Errorf("nextLink on last page = %q, want empty", got)
	}
	if got := nextLink(""); got != "" {
		t.Errorf("nextLink of empty header = %q, want empty", got)
	}
}
