package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func discoverUpdates(ctx context.Context, def Definition) []any {
	client := &http.Client{Timeout: 10 * time.Second}
	type source struct {
		component string
		url       string
		current   string
		field     string
	}
	sources := []source{
		{"uv", "https://api.github.com/repos/astral-sh/uv/releases/latest", def.Bundle.Lock.Tooling.UV.Version, "tag_name"},
		{"claude", "https://registry.npmjs.org/@anthropic-ai%2fclaude-code/latest", def.Bundle.Lock.Claude.Version, "version"},
		{"codex", "https://api.github.com/repos/openai/codex/releases/latest", def.Bundle.Lock.Codex.Version, "tag_name"},
		{"hermes", "https://api.github.com/repos/NousResearch/hermes-agent/releases/latest", def.Bundle.Lock.Hermes.Commit, "tag_name"},
		{"executor", "https://api.github.com/repos/RhysSullivan/executor/releases/latest", def.Bundle.Lock.Executor.LinuxARM64Digest, "tag_name"},
	}
	result := make([]any, 0, len(sources)+1)
	for _, item := range sources {
		candidate, err := fetchReleaseField(ctx, client, item.url, item.field)
		entry := map[string]any{"component": item.component, "current": item.current, "candidate": candidate, "qualified": false}
		if err != nil {
			entry["error"] = err.Error()
		}
		result = append(result, entry)
	}
	node, err := fetchNodeLTS(ctx, client)
	nodeEntry := map[string]any{"component": "node", "current": def.Bundle.Lock.Tooling.Node.Version, "candidate": node, "qualified": false}
	if err != nil {
		nodeEntry["error"] = err.Error()
	}
	return append(result, nodeEntry)
}

func fetchReleaseField(ctx context.Context, client *http.Client, url, field string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "hermes-box")
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("release discovery returned %s", response.Status)
	}
	var payload map[string]any
	if err := json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&payload); err != nil {
		return "", err
	}
	value, ok := payload[field].(string)
	if !ok || value == "" {
		return "", fmt.Errorf("release discovery omitted %s", field)
	}
	return value, nil
}

func fetchNodeLTS(ctx context.Context, client *http.Client) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://nodejs.org/dist/index.json", nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Node release discovery returned %s", response.Status)
	}
	var releases []struct {
		Version string `json:"version"`
		LTS     any    `json:"lts"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&releases); err != nil {
		return "", err
	}
	for _, release := range releases {
		if release.LTS != nil && release.LTS != false && release.Version != "" {
			return strings.TrimPrefix(release.Version, "v"), nil
		}
	}
	return "", fmt.Errorf("Node release discovery found no LTS release")
}
