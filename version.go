package main

// `fornax version` prints the build stamp; `fornax upgrade` checks the
// latest GitHub release. Release builds stamp version via
// -ldflags "-X main.version=$GITHUB_REF_NAME" (see .github/workflows/release.yml).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var version = "dev"

func cmdVersion(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fornax version")
	}
	fmt.Printf("fornax %s\n", version)
	fmt.Println(dim("llama.cpp " + engineVersion))
	return nil
}

func cmdUpgrade(ctx context.Context, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: fornax upgrade")
	}
	if version == "dev" {
		fmt.Println("dev build — upgrade tracks tagged releases only")
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/earshot-run/fornax/releases/latest", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "fornax")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach api.github.com: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, resp.Body)
		fmt.Println("no releases yet")
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("api.github.com answered HTTP %d", resp.StatusCode)
	}
	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxHTTPBody)).Decode(&rel); err != nil {
		return fmt.Errorf("the release reply was not readable: %w", err)
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	current := strings.TrimPrefix(version, "v")
	switch {
	case latest == "":
		fmt.Println("no releases yet")
	case latest == current:
		fmt.Printf("fornax is up to date (v%s)\n", current)
	case compareVersions(latest, current) > 0:
		fmt.Printf("v%s available — go install github.com/earshot-run/fornax@v%s\n", latest, latest)
		if rel.HTMLURL != "" {
			fmt.Println(dim(rel.HTMLURL))
		}
	default:
		fmt.Printf("a different release exists (v%s, you run v%s)\n", latest, current)
		if rel.HTMLURL != "" {
			fmt.Println(dim(rel.HTMLURL))
		}
	}
	return nil
}

// Semver-ish ordering: 1.10.0 > 1.9.0. Non-numeric segments compare as
// strings — good enough to know "newer", never silent about "different".
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		if xi, xerr := strconv.Atoi(x); xerr == nil {
			if yi, yerr := strconv.Atoi(y); yerr == nil {
				if xi != yi {
					if xi < yi {
						return -1
					}
					return 1
				}
				continue
			}
		}
		if x != y {
			return strings.Compare(x, y)
		}
	}
	return 0
}
