package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/mux"
	gitlab "github.com/xanzy/go-gitlab"
)

const usage = `
Usage: mr.mirror [OPTIONS] branch=downstream ...

A bot to cherry pick gitlab merge request to downstream branch

Options:
	--listen		listen at, default :80
	--gitlab-url		gitlab base url for gitlab api, E.g. "https://gitlab.com"
	--gitlab-token		gitlab access token for gitlab api, create via GitLab > User Settings > Access Tokens
	--logging-file		logging file, default /var/log/mr.mirror.log
`

var (
	listen        string
	loggingFile   string
	gitlabBaseURL string
	gitlabToken   string
	mappings      map[string]string = make(map[string]string)
	client        *gitlab.Client
)

// var mappings = map[string]string{
// 	"Branch_v2.0": "MASTER",
// 	"MASTER":      "Branch_v2.1.1",
// }

func init() {
	flag.StringVar(&listen, "listen", ":80", "listen at, default :80")
	flag.StringVar(&loggingFile, "logging-file", "/var/log/mr.mirror.log", "logging file, default /var/log/mr.mirror.log")
	flag.StringVar(&gitlabBaseURL, "gitlab-url", "", "gitlab base url for gitlab api, E.g. \"https://gitlab.com\"")
	flag.StringVar(&gitlabToken, "gitlab-token", "", "gitlab access token for gitlab api, create via GitLab > User Settings > Access Tokens")
}

func newGitLabClient(baseURL, token string) *gitlab.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{
		// Timeout:   30 * time.Second,
		Transport: tr,
	}
	client := gitlab.NewClient(httpClient, token)
	err := client.SetBaseURL(baseURL)
	if err != nil {
		panic(err)
	}
	return client
}

func main() {
	var help = flag.Bool("help", false, "show usage")
	flag.Parse()
	for _, str := range flag.Args() {
		pair := strings.Split(str, "=")
		if len(pair) == 2 {
			mappings[pair[0]] = pair[1]
		}
	}

	if *help || gitlabBaseURL == "" || gitlabToken == "" || len(mappings) == 0 {
		fmt.Println(usage)
		os.Exit(0)
	}

	client = newGitLabClient(gitlabBaseURL, gitlabToken)
	r := mux.NewRouter()
	r.HandleFunc("/mergerequests", func(w http.ResponseWriter, r *http.Request) {
		var mr gitlab.MergeEvent
		var err error
		if mr, err = parseMergeRequestEvent(r); err != nil {
			writeString(w, "ignored", 200)
			return
		}

		mrIID := mr.ObjectAttributes.IID
		mrTargetBranch := mr.ObjectAttributes.TargetBranch
		mrAction := mr.ObjectAttributes.Action
		pickIntoBranch := mappings[mrTargetBranch]
		if (mrAction != "open" && mrAction != "reopen") || pickIntoBranch == "" {
			log.Printf("ignore merge request[%d] event{action=%s, target branch=%s}", mrIID, mrAction, mrTargetBranch)
			writeString(w, "ignored", 200)
			return
		}
		log.Printf("start cherry pick mr: %s", mr.ObjectAttributes.URL)

		var workingBranch *gitlab.Branch
		if workingBranch, err = prepareBranch(mr, pickIntoBranch); err != nil {
			log.Printf("fail to prepare branch for cherry picking merge request[%d] into %s: %v", mrIID, pickIntoBranch, err)
			writeString(w, "failed", 500)
			return
		}
		log.Printf("working branch %s has prepared for cherry pick merge request[%d](%s) into %s", workingBranch.Name, mrIID, mr.ObjectAttributes.Title, pickIntoBranch)

		if applied, failed, err := cherryPickMergeRequest(mr, workingBranch.Name); err != nil {
			// create Issue
			log.Printf("cherry pick merge request[%d](%s) into %s failed: %v", mrIID, mr.ObjectAttributes.Title, pickIntoBranch, err)
			if _, err := createIssue(mr, pickIntoBranch, workingBranch.Name, applied, failed, err); err != nil {
				log.Printf("cherry pick merge request[%d](%s) into %s failed without issue: %v", mrIID, mr.ObjectAttributes.Title, pickIntoBranch, err)
			} else {
				log.Printf("issue created for the failure")
			}
		} else {
			// create MergeRequest
			if _, err := createMergeRequest(mr, workingBranch.Name, pickIntoBranch); err != nil {
				log.Printf("cherry pick merge request[%d](%s) into %s succeed, but fail to create merge request: %v", mrIID, mr.ObjectAttributes.Title, pickIntoBranch, err)
			} else {
				log.Printf("cherry pick merge request[%d](%s) into %s succeed", mrIID, mr.ObjectAttributes.Title, pickIntoBranch)
			}
		}

		writeString(w, "ok", 200)
	})

	srv := &http.Server{
		Handler:      r,
		Addr:         listen,
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}
	fmt.Println("start mr.mirror at ", listen)
	log.Fatal(srv.ListenAndServe())
}

func writeString(w http.ResponseWriter, txt string, code int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	w.Write([]byte(txt))
}

func parseMergeRequestEvent(r *http.Request) (gitlab.MergeEvent, error) {
	var event gitlab.MergeEvent
	defer r.Body.Close()
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return event, err
	}

	if err = json.Unmarshal(body, &event); err != nil {
		return event, err
	}
	return event, nil
}

func prepareBranch(mr gitlab.MergeEvent, ref string) (*gitlab.Branch, error) {
	projectID := mr.ObjectAttributes.SourceProjectID
	branchName := fmt.Sprintf("%s_for_%s", mr.ObjectAttributes.SourceBranch, ref)
	branch, _, err := client.Branches.CreateBranch(projectID, &gitlab.CreateBranchOptions{
		Branch: gitlab.String(branchName),
		Ref:    gitlab.String(ref),
	})
	return branch, err
}

func cherryPickMergeRequest(mr gitlab.MergeEvent, branch string) (applied, failed []*gitlab.Commit, err error) {
	projectID := mr.ObjectAttributes.SourceProjectID

	var commits []*gitlab.Commit
	if commits, err = getCommits(mr); err != nil {
		return nil, nil, err
	}
	reverse(commits)
	if i, err := cherryPickCommits(projectID, commits, branch); err != nil {
		return commits[:i], commits[i:], err
	}
	return commits, nil, nil
}

func getCommits(mr gitlab.MergeEvent) ([]*gitlab.Commit, error) {
	projectID := mr.ObjectAttributes.SourceProjectID
	mrIID := mr.ObjectAttributes.IID
	if commits, _, err := client.MergeRequests.GetMergeRequestCommits(projectID, mrIID, &gitlab.GetMergeRequestCommitsOptions{}); err != nil {
		return nil, err
	} else {
		return commits, nil
	}
}

func reverse(commits []*gitlab.Commit) {
	for l, r := 0, len(commits)-1; l < r; l, r = l+1, r-1 {
		commits[l], commits[r] = commits[r], commits[l]
	}
}

func cherryPickCommits(projectID interface{}, commits []*gitlab.Commit, branch string) (failAt int, err error) {
	for i, commit := range commits {
		_, _, err := client.Commits.CherryPickCommit(projectID, commit.ID, &gitlab.CherryPickCommitOptions{
			TargetBranch: gitlab.String(branch),
		})
		if err != nil {
			return i, err
		}
	}
	return len(commits), nil
}

func createIssue(mr gitlab.MergeEvent, pickInto, workingOn string, applied []*gitlab.Commit, failed []*gitlab.Commit, err error) (*gitlab.Issue, error) {
	title := fmt.Sprintf("Fail to cherry pick MR[%d](%s) into %s", mr.ObjectAttributes.IID, mr.ObjectAttributes.Title, pickInto)
	var desc strings.Builder
	desc.WriteString(fmt.Sprintf("**Merge Request:** %s\n\n", mr.ObjectAttributes.URL))
	desc.WriteString(fmt.Sprintf("**Target Branch:** %s\n\n", pickInto))
	desc.WriteString(fmt.Sprintf("**Working Branch:** %s\n\n", workingOn))
	desc.WriteString(fmt.Sprintf("**Details:** %d/%d commit(s) failed\n\n", len(failed), len(applied)+len(failed)))
	for _, commit := range applied {
		desc.WriteString(fmt.Sprintf("  *  %s [applied]\n", commit.ID))
	}
	desc.WriteString(fmt.Sprintf("  * %s [failed]: %s\n", failed[0].ID, err.Error()))
	for _, commit := range failed[1:] {
		desc.WriteString(fmt.Sprintf("  * %s [skipped]\n", commit.ID))
	}
	issue, _, err := client.Issues.CreateIssue(mr.ObjectAttributes.SourceProjectID, &gitlab.CreateIssueOptions{
		Title:                              gitlab.String(title),
		Description:                        gitlab.String(desc.String()),
		AssigneeIDs:                        []int{mr.ObjectAttributes.AuthorID},
		MergeRequestToResolveDiscussionsOf: gitlab.Int(mr.ObjectAttributes.IID),
	})
	return issue, err
}

func createMergeRequest(event gitlab.MergeEvent, source, target string) (*gitlab.MergeRequest, error) {
	mr, _, err := client.MergeRequests.CreateMergeRequest(event.ObjectAttributes.TargetProjectID, &gitlab.CreateMergeRequestOptions{
		Title:              gitlab.String(fmt.Sprintf("CherryPick - %s", event.ObjectAttributes.Title)),
		Description:        gitlab.String(event.ObjectAttributes.Description),
		SourceBranch:       gitlab.String(source),
		TargetBranch:       gitlab.String(target),
		AssigneeID:         gitlab.Int(event.ObjectAttributes.AuthorID),
		TargetProjectID:    gitlab.Int(event.ObjectAttributes.TargetProjectID),
		RemoveSourceBranch: gitlab.Bool(true),
	})
	return mr, err
}
