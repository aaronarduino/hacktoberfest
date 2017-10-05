package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/pkg/errors"
)

// Issue is a requested change against one of our tracked GitHub repos.
type Issue struct {
	Title     string
	Date      time.Time
	URL       string
	Repo      Repo
	Languages []string
}

func issues(w http.ResponseWriter, r *http.Request) {
	u, _, ok := findUser(r)
	if !ok {
		http.Error(w, "you are not logged in", http.StatusUnauthorized)
		return
	}

	issues, err := fetchIssues(r.Context(), u.AccessToken)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(issues); err != nil {
		log.Println(err)
	}
}

// fetchIssues makes concurrent requests to the search api to get issues with
// particular labels. Their API won't let us search for something label:A OR
// label:B only label:A AND label:B so we have to make multiple requests.
func fetchIssues(ctx context.Context, token string) ([]Issue, error) {

	// Kick off a worker for each of these labels
	labels := []string{"hacktoberfest", "help wanted"}

	// main chan where workers send their results
	ch := make(chan Issue)

	// errors is where workers will report failure. It has to have sufficient
	// buffer space to prevent deadlocks because we only receive from it once
	errors := make(chan error, len(labels))

	// cCtx is a new context derived from our own. We use it to signal workers to
	// stop early in the case of an error.
	cCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(len(labels))
	for _, l := range labels {
		go func(l string) {
			if err := issueSearch(cCtx, l, token, ch); err != nil {
				errors <- err
			}
			wg.Done()
		}(l)
	}

	// When all searches are done close the channel so we stop trying to read it
	go func() {
		wg.Wait()
		close(ch)
	}()

	var issues []Issue
	for {
		select {

		// One of the workers failed so cancel the others and pass the error up
		case err := <-errors:
			cancel()
			return nil, err

		// Read from ch. If it was closed then we know we're done reading so dedupe
		// our results and send them up. If it was open just append the value.
		case i, open := <-ch:
			if !open {
				return dedupe(issues), nil
			}
			issues = append(issues, i)
		}
	}
}

// dedupe returns only the unique values from the issues provided. It uses the
// URL field for identity.
func dedupe(in []Issue) []Issue {
	uniq := []Issue{}
	seen := make(map[string]int)
	for _, i := range in {
		if seen[i.URL] == 0 {
			uniq = append(uniq, i)
		}
		seen[i.URL]++
	}
	return uniq
}

// issueSearch makes a single request to the github search api. Issues are fed
// into ch as they are found. An error is returned if we could not complete the
// request or GitHub responds with anything but a 200. A ctx is provided so we
// know if we need to quit early.
func issueSearch(ctx context.Context, label, token string, ch chan<- Issue) error {
	ctx.Done()

	req, err := http.NewRequest("GET", "https://api.github.com/search/issues", nil)
	if err != nil {
		return errors.Wrap(err, "could not build request")
	}

	// Tell the request to use our context so we can cancel it in-flight if needed
	req = req.WithContext(ctx)

	q := fmt.Sprintf(`is:open type:issue label:"%s"`, label)
	for k := range orgs {
		q += " org:" + k
	}
	for k := range projects {
		q += " repo:" + k
	}

	vals := req.URL.Query()
	vals.Add("q", q)
	vals.Add("sort", "updated")
	vals.Add("order", "asc")
	vals.Add("per_page", "100")
	req.URL.RawQuery = vals.Encode()

	// Use their access token so it counts against their rate limit
	if token != "" {
		req.Header.Add("Authorization", "token "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "could not execute request")
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return errors.Wrapf(err, "status was %d, not 200", resp.StatusCode)
	}

	var data struct {
		Items []struct {
			Title     string    `json:"title"`
			CreatedAt time.Time `json:"created_at"`
			URL       string    `json:"url"`
			RepoURL   string    `json:"repository_url"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return errors.Wrap(err, "could not decode json")
	}

	for _, item := range data.Items {
		lf := newLanguageFetcher()
		languages, err := lf.repoLanguages(ctx, item.RepoURL, token)
		if err != nil {
			return err
		}

		issue := Issue{
			Title:     item.Title,
			Date:      item.CreatedAt,
			URL:       item.URL,
			Languages: languages,
		}

		issue.Repo, err = repoFromURL(item.RepoURL)
		if err != nil {
			return errors.Wrapf(err, "could not identify repo from %s", item.RepoURL)
		}

		select {

		// Stop early because another worker failed
		case <-ctx.Done():
			return nil

		// Send our issue on ch if we can
		case ch <- issue:
		}
	}
	return nil
}

type languageFetcher struct {
	fetchedRepos map[string][]string
}

func newLanguageFetcher() *languageFetcher {
	return &languageFetcher{
		fetchedRepos: make(map[string][]string),
	}
}

func (lf *languageFetcher) repoLanguages(ctx context.Context, repoURL, token string) ([]string, error) {
	// Return cached languages, if all ready fetched from repo.
	if len(lf.fetchedRepos[repoURL]) > 0 {
		return lf.fetchedRepos[repoURL], nil
	}

	// If not cached, get languages from repo.
	req, err := http.NewRequest("GET", fmt.Sprintf("%v/languages", repoURL), nil)
	if err != nil {
		return nil, errors.Wrap(err, "could not build request")
	}

	// Tell the request to use our context so we can cancel it in-flight if needed
	req = req.WithContext(ctx)

	// Use their access token so it counts against their rate limit
	if token != "" {
		req.Header.Add("Authorization", "token "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "could not execute request")
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, errors.Wrapf(err, "status was %d, not 200", resp.StatusCode)
	}
	data := make(map[string]int)
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, errors.Wrap(err, "could not decode json")
	}

	// Do golang limbo to get sorted languages.
	var langs []string
	var keys []int
	sortMap := make(map[int]string)
	for k, v := range data {
		keys = append(keys, v)
		sortMap[v] = k
	}
	sort.Sort(sort.Reverse(sort.IntSlice(keys)))

	// Get top three langs.
	for i, k := range keys {
		if i > 2 {
			break
		}
		langs = append(langs, sortMap[k])
	}

	// Cache repo languages.
	lf.fetchedRepos[repoURL] = langs
	return langs, nil
}
