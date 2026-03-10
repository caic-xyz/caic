// Package github implements forge.Forge for github.com using the GitHub REST API.
// This file provides signature verification and payload types for GitHub webhook events.
package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// VerifySignature verifies the HMAC-SHA256 signature of a GitHub webhook payload.
// sig must be in the format "sha256=<hex>".
func VerifySignature(secret, body []byte, sig string) error {
	prefix, hexSig, ok := strings.Cut(sig, "=")
	if !ok || prefix != "sha256" {
		return fmt.Errorf("webhook signature: invalid format %q (expected sha256=<hex>)", sig)
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return fmt.Errorf("webhook signature: invalid hex: %w", err)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := mac.Sum(nil)
	if subtle.ConstantTimeCompare(got, expected) != 1 {
		return errors.New("webhook signature: HMAC mismatch")
	}
	return nil
}

// WebhookInstallation carries the GitHub App installation ID from a webhook payload.
type WebhookInstallation struct {
	ID int64 `json:"id"`
}

// WebhookRepo carries repository identity from a webhook payload.
type WebhookRepo struct {
	FullName string `json:"full_name"` // "owner/repo"
}

// WebhookUser carries user identity from a webhook payload.
type WebhookUser struct {
	Login string `json:"login"`
}

// WebhookLabel carries a label from a webhook payload.
type WebhookLabel struct {
	Name string `json:"name"`
}

// WebhookIssue carries the issue fields used from webhook payloads.
type WebhookIssue struct {
	Number  int            `json:"number"`
	Title   string         `json:"title"`
	Body    string         `json:"body"`
	Labels  []WebhookLabel `json:"labels"`
	User    WebhookUser    `json:"user"`
	HTMLURL string         `json:"html_url"`
}

// WebhookPR carries the pull request fields used from webhook payloads.
type WebhookPR struct {
	Number  int         `json:"number"`
	Title   string      `json:"title"`
	Body    string      `json:"body"`
	User    WebhookUser `json:"user"`
	HTMLURL string      `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"` // branch name
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"` // target branch
	} `json:"base"`
}

// WebhookComment carries the comment fields used from webhook payloads.
type WebhookComment struct {
	Body    string      `json:"body"`
	User    WebhookUser `json:"user"`
	HTMLURL string      `json:"html_url"`
}

// IssuesEvent is the payload for X-GitHub-Event: issues.
type IssuesEvent struct {
	Action       string              `json:"action"`
	Issue        WebhookIssue        `json:"issue"`
	Repository   WebhookRepo         `json:"repository"`
	Installation WebhookInstallation `json:"installation"`
}

// PullRequestEvent is the payload for X-GitHub-Event: pull_request.
type PullRequestEvent struct {
	Action       string              `json:"action"`
	PullRequest  WebhookPR           `json:"pull_request"`
	Repository   WebhookRepo         `json:"repository"`
	Installation WebhookInstallation `json:"installation"`
}

// IssueCommentEvent is the payload for X-GitHub-Event: issue_comment.
type IssueCommentEvent struct {
	Action       string              `json:"action"`
	Issue        WebhookIssue        `json:"issue"`
	Comment      WebhookComment      `json:"comment"`
	Repository   WebhookRepo         `json:"repository"`
	Installation WebhookInstallation `json:"installation"`
}

// InstallationEvent is the payload for X-GitHub-Event: installation.
type InstallationEvent struct {
	Action       string `json:"action"` // "created", "deleted", "suspend", "unsuspend"
	Installation struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
			Type  string `json:"type"` // "User" or "Organization"
		} `json:"account"`
	} `json:"installation"`
}

// CheckSuiteEvent is the payload for X-GitHub-Event: check_suite.
type CheckSuiteEvent struct {
	Action     string `json:"action"`
	CheckSuite struct {
		HeadSHA    string `json:"head_sha"`
		HeadBranch string `json:"head_branch"`
		Conclusion string `json:"conclusion"` // "success", "failure", "neutral", etc.
	} `json:"check_suite"`
	Repository   WebhookRepo         `json:"repository"`
	Installation WebhookInstallation `json:"installation"`
}
