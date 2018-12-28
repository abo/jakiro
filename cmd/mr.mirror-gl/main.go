package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	gitlab "github.com/xanzy/go-gitlab"
	// "gopkg.in/yaml.v2"
)

// const (
// 	version        = "0.1"
// 	defaultPort    = "8888"
// 	defaultLogFile = "/var/log/mr.mirror.log"
// 	usage          = `mr.mirror-gl mirror merge request for gitlab`
// )

// var (
// 	listen  string
// 	logPath string
// )

// func init() {
// 	flag.StringVar(&httpprof, "httpprof", "", "pprof listen at")
// 	flag.StringVar(&listen, "listen", defaultListen, "listen at, default "+defaultListen)
// 	flag.StringVar(&logFilePath, "logfile", defaultLogFile, "log to file")
// }

const (
	gitlabBaesURL string = "https://118.190.200.88"
	gitlabToken   string = "aeiQ5whk3jx8ySWm2Qys"
	listenAddr    string = ":8888"
)

var client *gitlab.Client
var mappings = map[string]string{
	"Branch_v2.0": "MASTER",
	"MASTER":      "Branch_v2.1.1",
}

func init() {
	client = newGitLabClient(gitlabBaesURL, gitlabToken)
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
	// flag.Parse()

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
			if _, err := createIssue(mr, pickIntoBranch, workingBranch.Name, applied, failed); err != nil {
				log.Printf("cherry pick merge request[%d](%s) into %s failed without issue: %v", mrIID, mr.ObjectAttributes.Title, pickIntoBranch, err)
			} else {
				log.Printf("issue created when cherry pick merge request[%d](%s) into %s failed", mrIID, mr.ObjectAttributes.Title, pickIntoBranch)
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
		Addr:         listenAddr,
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}
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

func createIssue(mr gitlab.MergeEvent, pickInto, workingOn string, applied []*gitlab.Commit, failed []*gitlab.Commit) (*gitlab.Issue, error) {
	title := fmt.Sprintf("Fail to cherry pick MR[%d](%s) into %s", mr.ObjectAttributes.IID, mr.ObjectAttributes.Title, pickInto)
	var desc strings.Builder
	desc.WriteString(fmt.Sprintf("**Merge Request:** %s\n\n", mr.ObjectAttributes.URL))
	desc.WriteString(fmt.Sprintf("**Target Branch:** %s\n\n", pickInto))
	desc.WriteString(fmt.Sprintf("**Working Branch:** %s\n\n", workingOn))
	desc.WriteString(fmt.Sprintf("**Details:** %d/%d commit(s) failed\n\n", len(failed), len(applied)+len(failed)))
	for _, commit := range applied {
		desc.WriteString(fmt.Sprintf("  *  %s applied\n", commit.ID))
	}
	desc.WriteString(fmt.Sprintf("  * %s failed\n", failed[0].ID))
	for _, commit := range failed[1:] {
		desc.WriteString(fmt.Sprintf("  * %s skipped\n", commit.ID))
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

// yml
// MASTER => Branch_v2.1.1
// Branch_v2.0 => MASTER
// branch name

// projectID := mr.ObjectAttributes.SourceProjectID
// // commits, _, err := client.MergeRequests.GetMergeRequestCommits(projectID, mr.ObjectAttributes.IID, &gitlab.GetMergeRequestCommitsOptions{})
// // if err != nil {
// // 	// log.Printf("fail to get mergerequest's commits: %v", err)
// // 	// writeString(w, err.Error(), 500)
// // 	// return
// // 	log.Fatal(err)
// // }

// log.Printf("merge request %d (%s) has %d commit(s)", mr.ObjectAttributes.ID, mr.ObjectAttributes.Title, len(commits))

// if err != nil {
// 	log.Fatal(err)
// }
// log.Printf("branch %s created for cherry pick merge request %d (%s)'s commit(s)", branch.Name, mr.ObjectAttributes.ID, mr.ObjectAttributes.Title)

// if i, err := cherryPickCommits(projectID, commits, branch.Name); err != nil {
// 	client.MergeRequests.CreateMergeRequest(projectID, &gitlab.CreateMergeRequestOptions{
// 		Title:              gitlab.String(fmt.Sprintf("MIRROR:%s", mr.ObjectAttributes.Title)),
// 		Description:        gitlab.String(mr.ObjectAttributes.Description),
// 		SourceBranch:       gitlab.String(branch.Name),
// 		TargetBranch:       gitlab.String(applyToBranch),
// 		AssigneeID:         gitlab.Int(mr.ObjectAttributes.AuthorID),
// 		TargetProjectID:    gitlab.Int(mr.ObjectAttributes.TargetProjectID),
// 		RemoveSourceBranch: gitlab.Bool(true),
// 	})
// } else {
// 	issueTitle := "Fail to cherry pick %mergerequest into $branch"
// 	issueDesc := "branch xxxx -> commitid1 succeed. commitid2 failed."
// 	client.Issues.CreateIssue(projectID, &gitlab.CreateIssueOptions{
// 		Title:                              gitlab.String(issueTitle),
// 		Description:                        gitlab.String(issueDesc),
// 		AssigneeIDs:                        []int{mr.ObjectAttributes.AuthorID},
// 		MergeRequestToResolveDiscussionsOf: gitlab.Int(mr.ObjectAttributes.IID),
// 	})
// 	//TODO create ISSUE
// 	//TODO revert picked commits if error , or delete branch ?
// 	log.Printf("%d/%d commit(s) failed", i+1, len(commits))
// }
