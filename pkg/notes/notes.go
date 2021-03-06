/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package notes

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/google/go-github/v28/github"
	"github.com/nozzle/throttler"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	DefaultOrg  = "kubernetes"
	DefaultRepo = "kubernetes"

	// maxParallelRequests is the maximum parallel requests we shall make to the
	// GitHub API
	maxParallelRequests = 10
)

// ReleaseNote is the type that represents the total sum of all the information
// we've gathered about a single release note.
type ReleaseNote struct {
	// Commit is the SHA of the commit which is the source of this note. This is
	// also effectively a unique ID for release notes.
	Commit string `json:"commit"`

	// Text is the actual content of the release note
	Text string `json:"text"`

	// Markdown is the markdown formatted note
	Markdown string `json:"markdown"`

	// Docs is additional documentation for the release note
	Documentation []*Documentation `json:"documentation,omitempty"`

	// Author is the GitHub username of the commit author
	Author string `json:"author"`

	// AuthorURL is the GitHub URL of the commit author
	AuthorURL string `json:"author_url"`

	// PrURL is a URL to the PR
	PrURL string `json:"pr_url"`

	// PrNumber is the number of the PR
	PrNumber int `json:"pr_number"`

	// Areas is a list of the labels beginning with area/
	Areas []string `json:"areas,omitempty"`

	// Kinds is a list of the labels beginning with kind/
	Kinds []string `json:"kinds,omitempty"`

	// SIGs is a list of the labels beginning with sig/
	SIGs []string `json:"sigs,omitempty"`

	// Indicates whether or not a note will appear as a new feature
	Feature bool `json:"feature,omitempty"`

	// Indicates whether or not a note is duplicated across SIGs
	Duplicate bool `json:"duplicate,omitempty"`

	// ActionRequired indicates whether or not the release-note-action-required
	// label was set on the PR
	ActionRequired bool `json:"action_required,omitempty"`

	// Tags each note with a release version if specified
	// If not specified, omitted
	ReleaseVersion string `json:"release_version,omitempty"`
}

type Documentation struct {
	// A description about the documentation
	Description string `json:"description,omitempty"`

	// The url to be linked
	URL string `json:"url"`

	// Clssifies the link as something special, like a KEP
	Type DocType `json:"type"`
}

type DocType string

const (
	DocTypeExternal DocType = "external"
	DocTypeKEP      DocType = "KEP"
	DocTypeOfficial DocType = "official"
)

// ReleaseNotes is a map of PR numbers referencing notes.
// To avoid needless loops, we need to be able to reference things by PR
// When we have to merge old and new entries, we want to be able to override
// the old entries with the new ones efficiently.
type ReleaseNotes map[int]*ReleaseNote

// ReleaseNotesHistory is the sorted list of PRs in the commit history
type ReleaseNotesHistory []int

type Result struct {
	commit      *github.RepositoryCommit
	pullRequest *github.PullRequest
}

type Gatherer struct {
	Client  Client
	Context context.Context
	Org     string
	Repo    string
}

// ListReleaseNotes produces a list of fully contextualized release notes
// starting from a given commit SHA and ending at starting a given commit SHA.
func (g *Gatherer) ListReleaseNotes(
	branch,
	start,
	end,
	requiredAuthor,
	relVer string,
) (ReleaseNotes, ReleaseNotesHistory, error) {
	commits, err := g.ListCommits(branch, start, end)
	if err != nil {
		return nil, nil, err
	}

	results, err := g.ListCommitsWithNotes(commits)
	if err != nil {
		return nil, nil, err
	}

	dedupeCache := map[string]struct{}{}
	notes := make(ReleaseNotes)
	history := ReleaseNotesHistory{}
	for _, result := range results {
		if requiredAuthor != "" {
			if result.commit.GetAuthor().GetLogin() != requiredAuthor {
				continue
			}
		}

		note, err := g.ReleaseNoteFromCommit(result, relVer)
		if err != nil {
			logrus.
				WithField("err", err).
				WithField("sha", result.commit.GetSHA()).
				Error("getting the release note from commit while listing release notes")
			continue
		}

		// exclusionFilters is a list of regular expressions that match notes text that
		// are deemed to have no content and should NOT be added to release notes.
		exclusionFilters := []string{
			"^(?i)(none|n/a)$", // 'none' or 'n/a' case insensitive
		}
		excluded := false
		for _, filter := range exclusionFilters {
			match, err := regexp.MatchString(filter, strings.ToUpper(strings.TrimSpace(note.Text)))
			if err != nil {
				return nil, nil, err
			}
			if match {
				excluded = true
				logrus.
					WithField("sha", result.commit.GetSHA()).
					WithField("func", "ListReleaseNotes").
					WithField("filter", filter).
					Debug("Excluding notes that are deemed to have no content based on filter, and should NOT be added to release notes.")
				break
			}
		}
		if excluded {
			continue
		}

		if _, ok := dedupeCache[note.Text]; !ok {
			notes[note.PrNumber] = note
			history = append(history, note.PrNumber)
			dedupeCache[note.Text] = struct{}{}
		}
	}

	return notes, history, nil
}

// NoteTextFromString returns the text of the release note given a string which
// may contain the commit message, the PR description, etc.
// This is generally the content inside the ```release-note ``` stanza.
func NoteTextFromString(s string) (string, error) {
	exps := []*regexp.Regexp{
		// (?s) is needed for '.' to be matching on newlines, by default that's disabled
		// we need to match ungreedy 'U', because after the notes a `docs` block can occur
		regexp.MustCompile("(?sU)```release-note[s]?\\r\\n(?P<note>.+)\\r\\n```"),
		regexp.MustCompile("(?sU)```dev-release-note[s]?\\r\\n(?P<note>.+)"),
		regexp.MustCompile("(?sU)```\\r\\n(?P<note>.+)\\r\\n```"),
		regexp.MustCompile("(?sU)```release-note[s]?\n(?P<note>.+)\n```"),
	}

	for _, exp := range exps {
		match := exp.FindStringSubmatch(s)
		if len(match) == 0 {
			continue
		}
		result := map[string]string{}
		for i, name := range exp.SubexpNames() {
			if i != 0 && name != "" {
				result[name] = match[i]
			}
		}

		note := strings.ReplaceAll(result["note"], "#", "&#35;")
		note = strings.ReplaceAll(note, "\r", "")
		note = stripActionRequired(note)
		note = dashify(note)
		note = strings.TrimSpace(note)
		return note, nil
	}

	return "", errors.New("no matches found when parsing note text from commit string")
}

func DocumentationFromString(s string) []*Documentation {
	regex := regexp.MustCompile("(?s)```docs[\\r]?\\n(?P<text>.+)[\\r]?\\n```")
	match := regex.FindStringSubmatch(s)

	if len(match) < 1 {
		// Nothing found, but we don't require it
		return nil
	}

	result := []*Documentation{}
	text := match[1]
	text = stripStar(text)
	text = stripDash(text)

	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		const httpPrefix = "http"
		s := strings.SplitN(scanner.Text(), httpPrefix, 2)
		if len(s) != 2 {
			continue
		}
		description := strings.TrimRight(strings.TrimSpace(s[0]), " :-")
		urlString := httpPrefix + strings.TrimSpace(s[1])

		// Validate the URL
		parsedURL, err := url.Parse(urlString)
		if err != nil {
			continue
		}

		result = append(result, &Documentation{
			Description: description,
			URL:         urlString,
			Type:        classifyURL(parsedURL),
		})
	}

	return result
}

// classifyURL returns the correct DocType for the given url
func classifyURL(u *url.URL) DocType {
	// Kubernetes Enhancement Proposals (KEPs)
	if strings.Contains(u.Host, "github.com") &&
		strings.Contains(u.Path, "/kubernetes/enhancements/") {
		return DocTypeKEP
	}

	// Official documentation
	if strings.Contains(u.Host, "kubernetes.io") &&
		strings.Contains(u.Path, "/docs/") {
		return DocTypeOfficial
	}

	return DocTypeExternal
}

// ReleaseNoteFromCommit produces a full contextualized release note given a
// GitHub commit API resource.
func (g *Gatherer) ReleaseNoteFromCommit(result *Result, relVer string) (*ReleaseNote, error) {
	pr := result.pullRequest

	prBody := pr.GetBody()
	text, err := NoteTextFromString(prBody)
	if err != nil {
		return nil, err
	}
	documentation := DocumentationFromString(prBody)

	author := pr.GetUser().GetLogin()
	authorURL := fmt.Sprintf("https://github.com/%s", author)
	prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%d", g.Org, g.Repo, pr.GetNumber())
	IsFeature := HasString(LabelsWithPrefix(pr, "kind"), "feature")
	IsDuplicate := false
	sigsListPretty := prettifySigList(LabelsWithPrefix(pr, "sig"))
	noteSuffix := ""

	if IsActionRequired(pr) || IsFeature {
		if sigsListPretty != "" {
			noteSuffix = fmt.Sprintf("Courtesy of %s", sigsListPretty)
		}
	} else if len(LabelsWithPrefix(pr, "sig")) > 1 {
		IsDuplicate = true
	}

	indented := strings.ReplaceAll(text, "\n", "\n  ")
	markdown := fmt.Sprintf("%s ([#%d](%s), [@%s](%s))",
		indented, pr.GetNumber(), prURL, author, authorURL)

	if noteSuffix != "" {
		markdown = fmt.Sprintf("%s\n\n  %s", markdown, noteSuffix)
	}

	return &ReleaseNote{
		Commit:         result.commit.GetSHA(),
		Text:           text,
		Markdown:       markdown,
		Documentation:  documentation,
		Author:         author,
		AuthorURL:      authorURL,
		PrURL:          prURL,
		PrNumber:       pr.GetNumber(),
		SIGs:           LabelsWithPrefix(pr, "sig"),
		Kinds:          LabelsWithPrefix(pr, "kind"),
		Areas:          LabelsWithPrefix(pr, "area"),
		Feature:        IsFeature,
		Duplicate:      IsDuplicate,
		ActionRequired: IsActionRequired(pr),
		ReleaseVersion: relVer,
	}, nil
}

// ListCommits lists all commits starting from a given commit SHA and ending at
// a given commit SHA.
func (g *Gatherer) ListCommits(branch, start, end string) ([]*github.RepositoryCommit, error) {
	startCommit, _, err := g.Client.GetCommit(g.Context, g.Org, g.Repo, start)
	if err != nil {
		return nil, err
	}

	endCommit, _, err := g.Client.GetCommit(g.Context, g.Org, g.Repo, end)
	if err != nil {
		return nil, err
	}

	allCommits := &commitList{}

	worker := func(clo *github.CommitsListOptions) ([]*github.RepositoryCommit, *github.Response, error) {
		commits, resp, err := g.Client.ListCommits(g.Context, g.Org, g.Repo, clo)
		if err != nil {
			return nil, nil, err
		}
		return commits, resp, err
	}

	clo := github.CommitsListOptions{
		SHA:   branch,
		Since: startCommit.GetCommitter().GetDate(),
		Until: endCommit.GetCommitter().GetDate(),
		ListOptions: github.ListOptions{
			Page:    1,
			PerPage: 100,
		},
	}

	commits, resp, err := worker(&clo)
	if err != nil {
		return nil, err
	}
	allCommits.Add(commits)

	remainingPages := resp.LastPage - 1
	if remainingPages < 1 {
		return allCommits.List(), nil
	}

	t := throttler.New(maxParallelRequests, remainingPages)
	for page := 2; page <= resp.LastPage; page++ {
		clo := clo
		clo.ListOptions.Page = page

		go func() {
			commits, _, err := worker(&clo)
			if err == nil {
				allCommits.Add(commits)
			}
			t.Done(err)
		}()

		// abort all, if we got one error
		if t.Throttle() > 0 {
			break
		}
	}

	if err := t.Err(); err != nil {
		return nil, err
	}

	return allCommits.List(), nil
}

type commitList struct {
	sync.RWMutex
	list []*github.RepositoryCommit
}

func (l *commitList) Add(c []*github.RepositoryCommit) {
	l.Lock()
	defer l.Unlock()
	l.list = append(l.list, c...)
}

func (l *commitList) List() []*github.RepositoryCommit {
	l.RLock()
	defer l.RUnlock()
	return l.list
}

// noteExclusionFilters is a list of regular expressions that match commits
// that do NOT contain release notes. Notably, this is all of the variations of
// "release note none" that appear in the commit log.
var noteExclusionFilters = []*regexp.Regexp{
	// 'none','n/a','na' case insensitive with optional trailing
	// whitespace, wrapped in ``` with/without release-note identifier
	// the 'none','n/a','na' can also optionally be wrapped in quotes ' or "
	regexp.MustCompile("(?i)```(release-note[s]?\\s*)?('|\")?(none|n/a|na)?('|\")?\\s*```"),

	// This filter is too aggressive within the PR body and picks up matches unrelated to release notes
	// 'none' or 'n/a' case insensitive wrapped optionally with whitespace
	// regexp.MustCompile("(?i)\\s*(none|n/a)\\s*"),

	// simple '/release-note-none' tag
	regexp.MustCompile("/release-note-none"),
}

// Similarly, now that the known not-release-notes are filtered out, we can
// use some patterns to find actual release notes.
var noteInclusionFilters = []*regexp.Regexp{
	regexp.MustCompile("release-note"),
	regexp.MustCompile("Does this PR introduce a user-facing change?"),
}

func matchesFilter(msg string, filters []*regexp.Regexp) *regexp.Regexp {
	for _, filter := range filters {
		if filter.MatchString(msg) {
			return filter
		}
	}
	return nil
}

func matchesExcludeFilter(msg string) *regexp.Regexp {
	return matchesFilter(msg, noteExclusionFilters)
}
func matchesIncludeFilter(msg string) *regexp.Regexp {
	return matchesFilter(msg, noteInclusionFilters)
}

// ListCommitsWithNotes list commits that have release notes starting from a
// given commit SHA and ending at a given commit SHA. This function is similar
// to ListCommits except that only commits with tagged release notes are
// returned.
//TODO: This name does not make sense anymore
//TODO: Why is that method exported?
func (g *Gatherer) ListCommitsWithNotes(commits []*github.RepositoryCommit) (filtered []*Result, err error) {
	allResults := &resultList{}

	nrOfCommits := len(commits)

	// A note about prallelism:
	//
	// We make 2 different requests to GitHub further down the stack:
	// - If we find PR numbers in a commit message, we do one API call per found
	//   number. The assumption is, that this is mostly just one (or just a couple
	//   of) PRs. The calls to the API do run in serial right now.
	// - If we don't find a PR number in the commit message, we ask the API if
	//   GitHub knows about PRs that are connected to that specific commit. The
	//   assumption again is that this is either one or just a couple of PRs. The
	//   results probably fit into one GitHub result page. If not, and we need to
	//   query multiple times (paging), we currently also do that in serial.
	//
	// In case we parallelize the above mentioned API calls and the volume of
	// them is bigger than expected, we might go well above the
	// `maxParallelRequests` of parallel requests. In that case we probably
	// should introduce the throttler as a global concept (on the Gatherer or so)
	// and use that throttler for all API calls.
	t := throttler.New(maxParallelRequests, nrOfCommits)

	for i, commit := range commits {
		logrus.Infof(
			"starting to process commit %d of %d (%0.2f%%): %s",
			i+1, nrOfCommits, (float64(i+1)/float64(nrOfCommits))*100.0,
			commit.GetSHA(),
		)

		go func(commit *github.RepositoryCommit) {
			res, err := g.notesForCommit(commit)
			if err == nil && res != nil {
				allResults.Add(res)
			}
			t.Done(err)
		}(commit)

		if t.Throttle() > 0 {
			break
		}
	} // for range commits

	if err := t.Err(); err != nil {
		return nil, err
	}

	return allResults.List(), nil
}

func (g *Gatherer) notesForCommit(commit *github.RepositoryCommit) (*Result, error) {
	prs, err := g.PRsFromCommit(commit)
	if err != nil {
		if err == errNoPRIDFoundInCommitMessage || err == errNoPRFoundForCommitSHA {
			logrus.
				WithField("func", "ListCommitsWithNotes").
				Debugf("No matches found when parsing PR from commit sha %q", commit.GetSHA())
			return nil, nil
		}
		return nil, err
	}

	for _, pr := range prs {
		prBody := pr.GetBody()

		logrus.
			WithField("func", "ListCommitsWithNotes").
			WithField("pr no", pr.GetNumber()).
			WithField("pr body", pr.GetBody()).
			Debugf("Obtaining PR associated with commit sha %q", commit.GetSHA())

		if re := matchesExcludeFilter(prBody); re != nil {
			logrus.
				WithField("func", "ListCommitsWithNotes").
				WithField("filter", re.String()).
				Debug("Excluding notes for PR based on the exclusion filter.")
			// try next PR
			continue
		}

		if re := matchesIncludeFilter(prBody); re != nil {
			res := &Result{commit: commit, pullRequest: pr}
			logrus.
				WithField("func", "ListCommitsWithNotes").
				WithField("filter", re.String()).
				Debug("Including notes for PR based on the inclusion filter.")

			// TODO is this really intentional?
			// Do not test further PRs for this commmit as soon as one PR matched
			return res, nil
		}
	}

	return nil, nil
}

type resultList struct {
	sync.RWMutex
	list []*Result
}

func (l *resultList) Add(r *Result) {
	l.Lock()
	defer l.Unlock()
	l.list = append(l.list, r)
}

func (l *resultList) List() []*Result {
	l.RLock()
	defer l.RUnlock()
	return l.list
}

// PRsFromCommit return an API Pull Request struct given a commit struct. This is
// useful for going from a commit log to the PR (which contains useful info such
// as labels).
func (g *Gatherer) PRsFromCommit(commit *github.RepositoryCommit) (
	[]*github.PullRequest, error,
) {
	githubPRs, err := g.prsForCommitFromMessage(*commit.Commit.Message)
	if err != nil {
		logrus.
			WithField("err", err).
			WithField("sha", commit.GetSHA()).
			Debug("getting the pr numbers from commit message")
		return g.prsForCommitFromSHA(*commit.SHA)
	}
	return githubPRs, err
}

// LabelsWithPrefix is a helper for fetching all labels on a PR that start with
// a given string. This pattern is used often in the k/k repo and we can take
// advantage of this to contextualize release note generation with the kind, sig,
// area, etc labels.
func LabelsWithPrefix(pr *github.PullRequest, prefix string) []string {
	labels := []string{}
	for _, label := range pr.Labels {
		if strings.HasPrefix(*label.Name, prefix) {
			labels = append(labels, strings.TrimPrefix(*label.Name, prefix+"/"))
		}
	}
	return labels
}

// IsActionRequired indicates whether or not the release-note-action-required
// label was set on the PR.
func IsActionRequired(pr *github.PullRequest) bool {
	for _, label := range pr.Labels {
		if *label.Name == "release-note-action-required" {
			return true
		}
	}
	return false
}

func stripActionRequired(note string) string {
	expressions := []string{
		`(?i)\[action required\]\s`,
		`(?i)action required:\s`,
	}

	for _, exp := range expressions {
		re := regexp.MustCompile(exp)
		note = re.ReplaceAllString(note, "")
	}

	return note
}

func stripStar(note string) string {
	re := regexp.MustCompile(`(?i)\*\s`)
	return re.ReplaceAllString(note, "")
}

func stripDash(note string) string {
	re := regexp.MustCompile(`(?i)\-\s`)
	return re.ReplaceAllString(note, "")
}

func dashify(note string) string {
	return strings.ReplaceAll(note, "* ", "- ")
}

func HasString(a []string, x string) bool {
	for _, n := range a {
		if x == n {
			return true
		}
	}
	return false
}

// prsForCommitFromSHA retrieves the PR numbers for a commit given its sha
func (g *Gatherer) prsForCommitFromSHA(sha string) (prs []*github.PullRequest, err error) {
	plo := &github.PullRequestListOptions{
		State: "closed",
		ListOptions: github.ListOptions{
			Page:    1,
			PerPage: 100,
		},
	}
	prs, resp, err := g.Client.ListPullRequestsWithCommit(g.Context, g.Org, g.Repo, sha, plo)
	if err != nil {
		return nil, err
	}

	plo.ListOptions.Page++
	for plo.ListOptions.Page <= resp.LastPage {
		pResult, pResp, err := g.Client.ListPullRequestsWithCommit(g.Context, g.Org, g.Repo, sha, plo)
		if err != nil {
			return nil, err
		}
		prs = append(prs, pResult...)
		resp = pResp
		plo.ListOptions.Page++
	}

	if len(prs) == 0 {
		return nil, errNoPRFoundForCommitSHA
	}
	return prs, nil
}

func (g *Gatherer) prsForCommitFromMessage(commitMessage string) (prs []*github.PullRequest, err error) {
	prsNum, err := prsNumForCommitFromMessage(commitMessage)
	if err != nil {
		return nil, err
	}

	for _, pr := range prsNum {
		// Given the PR number that we've now converted to an integer, get the PR from
		// the API
		res, _, err := g.Client.GetPullRequest(g.Context, g.Org, g.Repo, pr)
		if err != nil {
			return nil, err
		}
		prs = append(prs, res)
	}

	return prs, err
}

func prsNumForCommitFromMessage(commitMessage string) (prs []int, err error) {
	// Thankfully k8s-merge-robot commits the PR number consistently. If this ever
	// stops being true, this definitely won't work anymore.
	regex := regexp.MustCompile(`Merge pull request #(?P<number>\d+)`)
	pr := prForRegex(regex, commitMessage)
	if pr != 0 {
		prs = append(prs, pr)
	}

	regex = regexp.MustCompile(`automated-cherry-pick-of-#(?P<number>\d+)`)
	pr = prForRegex(regex, commitMessage)
	if pr != 0 {
		prs = append(prs, pr)
	}

	// If the PR was squash merged, the regexp is different
	regex = regexp.MustCompile(`\(#(?P<number>\d+)\)`)
	pr = prForRegex(regex, commitMessage)
	if pr != 0 {
		prs = append(prs, pr)
	}

	if prs == nil {
		return nil, errNoPRIDFoundInCommitMessage
	}

	return prs, nil
}

func prForRegex(regex *regexp.Regexp, commitMessage string) int {
	result := map[string]string{}
	match := regex.FindStringSubmatch(commitMessage)

	if match == nil {
		return 0
	}

	for i, name := range regex.SubexpNames() {
		if i != 0 && name != "" {
			result[name] = match[i]
		}
	}

	pr, err := strconv.Atoi(result["number"])
	if err != nil {
		return 0
	}
	return pr
}

var errNoPRIDFoundInCommitMessage = errors.New("No PR IDs found in the commit message")
var errNoPRFoundForCommitSHA = errors.New("No PR found for this commit")
