package bitbucket

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	bbapi "github.com/reviewdog/go-bitbucket"
	"github.com/reviewdog/reviewdog"
	"github.com/reviewdog/reviewdog/proto/rdf"
	"google.golang.org/protobuf/proto"
)

var _ reviewdog.CommentService = &ReportAnnotator{}

const (
	// avatar from https://github.com/apps/reviewdog
	logoURL  = "https://avatars1.githubusercontent.com/in/12131"
	reporter = "reviewdog"
	// max amount of annotations in one batch call
	annotationsBatchSize = 100
)

// ReportAnnotator is a comment service for Bitbucket Code Insights reports.
//
// API:
//  https://developer.atlassian.com/bitbucket/api/2/reference/resource/repositories/%7Bworkspace%7D/%7Brepo_slug%7D/commit/%7Bcommit%7D/reports/%7BreportId%7D/annotations#post
//  POST /2.0/repositories/{username}/{repo_slug}/commit/{commit}/reports/{reportId}/annotations
type ReportAnnotator struct {
	cli         *bbapi.APIClient
	sha         string
	owner, repo string

	muAnnotations sync.Mutex
	// store annotations in map per tool name
	// so we can create report per tool
	annotations map[string][]bbapi.ReportAnnotation
	severityMap map[rdf.Severity]string

	// wd is working directory relative to root of repository.
	wd         string
	duplicates map[string]struct{}
}

// NewReportAnnotator creates new Bitbucket Report Annotator
func NewReportAnnotator(cli *bbapi.APIClient, owner, repo, sha string, runners []string) *ReportAnnotator {
	r := &ReportAnnotator{
		cli:         cli,
		sha:         sha,
		owner:       owner,
		repo:        repo,
		annotations: make(map[string][]bbapi.ReportAnnotation, len(runners)),
		severityMap: map[rdf.Severity]string{
			rdf.Severity_INFO:    annotationSeverityLow,
			rdf.Severity_WARNING: annotationSeverityMedium,
			rdf.Severity_ERROR:   annotationSeverityHigh,
		},
		duplicates: map[string]struct{}{},
	}

	// pre populate map of annotations, so we still create passed (green) report
	// if no issues found from the specific tool
	for _, runner := range runners {
		if len(runner) == 0 {
			continue
		}
		r.annotations[runner] = []bbapi.ReportAnnotation{}
		// create Pending report for each tool
		_ = r.createOrUpdateReport(context.Background(), reportID(runner, reporter), reportTitle(runner, reporter), reportResultPending)
	}

	return r
}

// Post accepts a comment and holds it. Flush method actually posts comments to
// Bitbucket in batch.
func (r *ReportAnnotator) Post(_ context.Context, c *reviewdog.Comment) error {
	c.Result.Diagnostic.GetLocation().Path = filepath.ToSlash(
		filepath.Join(r.wd, c.Result.Diagnostic.GetLocation().GetPath()))
	r.muAnnotations.Lock()
	defer r.muAnnotations.Unlock()

	anot := r.annotationFromReviewDogComment(*c)

	// deduplicate event, because some reporters might report
	// it twice, and bitbucket api will complain on duplicated
	// external id of annotation
	if _, ok := r.duplicates[*anot.ExternalId]; !ok {
		r.annotations[c.ToolName] = append(r.annotations[c.ToolName], anot)
	}

	r.duplicates[*anot.ExternalId] = struct{}{}
	return nil
}

// Flush posts comments which has not been posted yet.
func (r *ReportAnnotator) Flush(ctx context.Context) error {
	r.muAnnotations.Lock()
	defer r.muAnnotations.Unlock()

	// create/update/annotate report per tool
	for tool, annotations := range r.annotations {
		reportID := reportID(tool, reporter)
		title := reportTitle(tool, reporter)
		if len(annotations) == 0 {
			// if no annotation, create Passed report
			if err := r.createOrUpdateReport(ctx, reportID, title, reportResultPassed); err != nil {
				return err
			}
			// and move one
			continue
		}

		// create report or update report first, with the failed status
		if err := r.createOrUpdateReport(ctx, reportID, title, reportResultFailed); err != nil {
			return err
		}

		// send annotations in batches, because of the api max payload size limit
		for start, annCount := 0, len(annotations); start < annCount; start += annotationsBatchSize {
			end := start + annotationsBatchSize

			if end > annCount {
				end = annCount
			}

			// add annotations to the report
			_, resp, err := r.cli.ReportsApi.BulkCreateOrUpdateAnnotations(
				ctx, r.owner, r.repo, r.sha, reportID,
			).Body(annotations[start:end]).Execute()

			if err := checkAPIError(err, resp, http.StatusOK); err != nil {
				return fmt.Errorf("bitbucket.BulkCreateOrUpdateAnnotations: %s", err)
			}
		}
	}

	return nil
}

func (r *ReportAnnotator) annotationFromReviewDogComment(c reviewdog.Comment) bbapi.ReportAnnotation {
	a := bbapi.NewReportAnnotation()

	// TODO: allow providing different annotation types in future
	a.SetAnnotationType(annotationTypeCodeSmell)
	// hash the output of linter and use it as external id
	a.SetExternalId(externalIDFromDiagnostic(c.Result.Diagnostic))
	a.SetSummary(c.Result.Diagnostic.GetMessage())
	a.SetDetails(fmt.Sprintf(`[%s] %s`, c.ToolName, c.Result.Diagnostic.GetMessage()))
	a.SetLine(c.Result.Diagnostic.GetLocation().GetRange().GetStart().GetLine())
	a.SetPath(c.Result.Diagnostic.GetLocation().GetPath())
	if v, ok := r.severityMap[c.Result.Diagnostic.GetSeverity()]; ok {
		a.SetSeverity(v)
	}
	if link := c.Result.Diagnostic.GetCode().GetUrl(); link != "" {
		a.SetLink(link)
	}

	return *a
}

func (r *ReportAnnotator) createOrUpdateReport(ctx context.Context, id, title, reportStatus string) error {
	var report = bbapi.NewReport()
	report.SetTitle(title)
	// TODO: different report types?
	report.SetReportType(reportTypeBug)
	report.SetReporter(reporter)
	report.SetLogoUrl(logoURL)
	report.SetResult(reportStatus)
	switch reportStatus {
	case reportResultPassed:
		report.SetDetails("Great news! Reviewdog couldn't spot any issues!")
	case reportResultPending:
		report.SetDetails("Please wait for Reviewdog to finish checking your code for issues.")
	default:
		report.SetDetails("Woof-Woof! This report generated for you by reviewdog.")
	}

	_, resp, err := r.cli.ReportsApi.CreateOrUpdateReport(ctx, r.owner, r.repo, r.sha, id).Body(*report).Execute()

	if err := checkAPIError(err, resp, http.StatusOK); err != nil {
		return fmt.Errorf("bitbucket.CreateOrUpdateReport: %s", err)
	}

	return nil
}

func hash(b []byte) string {
	h := sha256.New()
	_, _ = h.Write(b)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// Note that it might be good to create external ID from the diagnostic
// content along with *original line* (by using git blame for example) to
// generate unique ID, but it hashes the Diagnostic message for simplicity.
func externalIDFromDiagnostic(d *rdf.Diagnostic) string {
	b, err := proto.Marshal(d)
	if err != nil {
		b = []byte(d.OriginalOutput)
	}
	return hash(b)
}

func reportID(ids ...string) string {
	return strings.ReplaceAll(strings.ToLower(strings.Join(ids, "-")), " ", "_")
}

func reportTitle(tool, reporter string) string {
	return fmt.Sprintf("[%s] %s report", tool, reporter)
}
