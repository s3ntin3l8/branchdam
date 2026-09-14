package agent

import "testing"

func TestExtractPathAgentSegment(t *testing.T) {
	cases := []struct {
		filePath  string
		rootPath  string
		wantAgent string
		wantOK    bool
	}{
		{"/virtual/resolve/agent-01/Master", "/virtual/resolve", "agent-01", true},
		{"/virtual/resolve/agent-01/My%20Doc", "/virtual/resolve", "agent-01", true},
		{"/virtual/resolve/My%20Documentary", "/virtual/resolve", "", false},
		{"/virtual/resolve", "/virtual/resolve", "", false},
		{"/virtual/resolve/", "/virtual/resolve", "", false},
		{"/virtual/resolve/agent-01", "/virtual/resolve", "", false},   // no trailing slash -> no further segments
		{"/other/path/agent-01/master", "/virtual/resolve", "", false}, // wrong root
	}
	for _, tc := range cases {
		t.Run(tc.filePath, func(t *testing.T) {
			agent, ok := extractPathAgentSegment(tc.filePath, tc.rootPath)
			if agent != tc.wantAgent || ok != tc.wantOK {
				t.Errorf("extractPathAgentSegment(%q,%q) = (%q,%v), want (%q,%v)",
					tc.filePath, tc.rootPath, agent, ok, tc.wantAgent, tc.wantOK)
			}
		})
	}
}
