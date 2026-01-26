package api

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v81/github"
)

var (
	ghAppID         = os.Getenv("GITHUB_APP_ID")
	ghAppPrivateKey = os.Getenv("GITHUB_APP_PRIVATE_KEY")
	ghWebhookSecret = os.Getenv("GITHUB_WEBHOOK_SECRET")
)

type Report struct {
	WorkflowName  string
	ArtifactName  string
	Filename      string
	Content       string
	CommentMarker string
}

func downloadAndExtractArtifact(ctx context.Context, client *github.Client, owner string, repo string, artifactID int64, artifactFilename string) (string, error) {
	downloadURL, _, err := client.Actions.DownloadArtifact(ctx, owner, repo, artifactID, 0)
	if err != nil {
		return "", fmt.Errorf("error getting download URL for artifact ID %d: %v", artifactID, err)
	}
	log.Printf("Downloading artifact ID %d from %s", artifactID, downloadURL.String())

	resp, err := http.Get(downloadURL.String())
	if err != nil {
		return "", fmt.Errorf("error downloading artifact: %v", err)
	}
	defer resp.Body.Close()

	zipData, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("error reading artifact zip: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return "", fmt.Errorf("error opening zip reader: %v", err)
	}
	for _, f := range reader.File {
		if f.Name == artifactFilename {
			file, err := f.Open()
			if err != nil {
				return "", fmt.Errorf("error opening file %s in zip: %v", artifactFilename, err)
			}
			defer file.Close()
			buf := new(bytes.Buffer)
			if _, err := io.Copy(buf, file); err != nil {
				return "", fmt.Errorf("error reading file %s from zip: %v", artifactFilename, err)
			}
			return buf.String(), nil
		}
	}
	return "", fmt.Errorf("%s not found in zip", artifactFilename)
}

func findAndExtractReportFromArtifacts(ctx context.Context, client *github.Client, owner string, repo string, reports []Report, runID int64) error {
	artifacts, _, err := client.Actions.ListWorkflowRunArtifacts(ctx, owner, repo, runID, nil)
	if err != nil {
		return fmt.Errorf("error listing artifacts: %v", err)
	}
	log.Printf("Found %d artifacts for workflow run %d", len(artifacts.Artifacts), runID)

	for _, artifact := range artifacts.Artifacts {
		for _, report := range reports {
			if strings.HasSuffix(artifact.GetName(), report.ArtifactName) {
				log.Printf("Found artifact %s (ID: %d)", artifact.GetName(), artifact.GetID())
				content, err := downloadAndExtractArtifact(ctx, client, owner, repo, artifact.GetID(), report.Filename)
				if err != nil {
					log.Printf("Error downloading and extracting artifact %s: %v", artifact.GetName(), err)
					continue
				}
				report.Content = content
				break
			}
		}
	}

	return nil
}

func upsertPRComment(ctx context.Context, client *github.Client, owner, repo string, prNumber int, report *Report) error {
	comments, _, err := client.Issues.ListComments(ctx, owner, repo, prNumber, nil)
	if err != nil {
		return err
	}
	var foundID int64
	for _, c := range comments {
		if c.Body != nil && strings.Contains(c.GetBody(), report.CommentMarker) {
			foundID = c.GetID()
			break
		}
	}
	body := report.CommentMarker + "\n" + report.Content
	if foundID == 0 {
		log.Printf("Creating new comment on PR #%d", prNumber)
		_, _, err := client.Issues.CreateComment(ctx, owner, repo, prNumber, &github.IssueComment{Body: &body})
		return err
	}
	log.Printf("Updating existing comment ID %d on PR #%d", foundID, prNumber)
	_, _, err = client.Issues.EditComment(ctx, owner, repo, foundID, &github.IssueComment{Body: &body})
	return err
}

func Index(w http.ResponseWriter, r *http.Request) {
	payload, err := github.ValidatePayload(r, []byte(ghWebhookSecret))
	if err != nil {
		log.Printf("Error validating webhook signature: %v", err)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	webhookType := github.WebHookType(r)
	webhookEvent, err := github.ParseWebHook(webhookType, payload)
	if err != nil {
		log.Printf("Error parsing webhook: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	event, ok := webhookEvent.(*github.WorkflowRunEvent)
	if !ok {
		log.Printf("Ignoring webhook event: not a workflow_run event")
		w.WriteHeader(http.StatusOK)
		return
	}

	var allReports = []Report{
		{
			WorkflowName:  "CI",
			ArtifactName:  "bloat-check-results",
			Filename:      "bloat-comparison.txt",
			CommentMarker: "<!-- prek-bloat-check -->",
		},
	}

	var reports []Report
	workflowName := event.GetWorkflowRun().GetName()
	for _, report := range allReports {
		if report.WorkflowName == workflowName {
			reports = append(reports, report)
		}
	}
	if len(reports) == 0 {
		log.Printf("Ignoring webhook event: workflow_nam= %s", workflowName)
		w.WriteHeader(http.StatusOK)
		return
	}
	if event.GetAction() != "completed" {
		log.Printf("Ignoring webhook event: action=%s", event.GetAction())
		w.WriteHeader(http.StatusOK)
		return
	}

	log.Printf("Workflow job completed: run_id=%d, workflow_name=%s", event.WorkflowRun.GetID(), event.WorkflowRun.GetName())
	ctx := context.Background()

	appID, err := strconv.ParseInt(ghAppID, 10, 64)
	if err != nil {
		log.Printf("Invalid GITHUB_APP_ID: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	installationID := event.GetInstallation().GetID()
	tr, err := ghinstallation.New(http.DefaultTransport, appID, installationID, []byte(ghAppPrivateKey))
	if err != nil {
		log.Printf("Error creating ghinstallation transport: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	client := github.NewClient(&http.Client{Transport: tr})

	owner := event.Repo.Owner.GetLogin()
	repo := event.Repo.GetName()
	err = findAndExtractReportFromArtifacts(ctx, client, owner, repo, reports, event.WorkflowRun.GetID())
	if err != nil {
		log.Printf("Error extracting artifacts error: %v", err)
		w.WriteHeader(http.StatusOK)
		return
	}
	log.Printf("Extracted report from artifacts")

	var prNumber int
	if len(event.WorkflowRun.PullRequests) > 0 {
		prNumber = event.WorkflowRun.PullRequests[0].GetNumber()
	} else {
		log.Printf("No PR found in workflow_run event")
		w.WriteHeader(http.StatusOK)
		return
	}

	for _, report := range reports {
		if report.Content == "" {
			log.Printf("No content found for report: workflow_name=%s, artifact_name=%s", report.WorkflowName, report.ArtifactName)
			continue
		}
		log.Printf("Upserting comment for PR #%d, workflow_name=%s", prNumber, report.WorkflowName)
		if err := upsertPRComment(ctx, client, owner, repo, prNumber, &report); err != nil {
			log.Printf("Error upserting PR comment: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}
