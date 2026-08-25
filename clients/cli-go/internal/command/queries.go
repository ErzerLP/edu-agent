package command

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

func (a *App) runRoute(ctx context.Context, args []string) error {
	set := newFlagSet("route")
	var flags onlineFlags
	var history bool
	var limit int
	var cursor string
	addOnlineFlags(set, &flags)
	set.BoolVar(&history, "history", false, "show route history")
	set.IntVar(&limit, "limit", defaultPageLimit, "page size")
	set.StringVar(&cursor, "cursor", "", "opaque page cursor")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "route accepts only connection, history, limit, and cursor flags", "run edu-agent route", ExitInput)
	}
	if err := validatePageInput(limit, cursor); err != nil {
		return err
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	view, active, err := a.currentSession(ctx, online.client)
	if err != nil {
		return err
	}
	if active && !history {
		printProjectionWarning(a.Err, view.Metadata)
		if view.WorkItem == nil || view.WorkItem.RouteRevision == nil {
			_, err = fmt.Fprintln(a.Out, "Current: active session has no route yet")
			return err
		}
		printRoute(a.Out, *view.WorkItem.RouteRevision, true, view.Session.Focus.RouteStepID)
		return nil
	}
	page, err := a.routesPage(ctx, online.client, cursor, limit, !history)
	if err != nil {
		return err
	}
	printProjectionWarning(a.Err, page.Metadata)
	if !history {
		_, _ = fmt.Fprintln(a.Out, "Current: one current revision per route ID; this is not one account-wide current route")
	}
	for _, item := range page.Items {
		printRoute(a.Out, item.Route, item.Current, "")
		_, _ = fmt.Fprintf(a.Out, "Event: %d\n", item.EventSeq)
	}
	if page.NextCursor != "" {
		_, _ = fmt.Fprintf(a.Out, "warning[truncated]: more routes are available next_cursor=%s\n", safeText(page.NextCursor))
	}
	return nil
}

func (a *App) routesPage(ctx context.Context, client APIClient, cursor string, limit int, currentOnly bool) (api.RoutesPage, error) {
	page, _, err := a.routesPageWithRestart(ctx, client, cursor, limit, currentOnly, true)
	return page, err
}

func (a *App) routesPageWithRestart(ctx context.Context, client APIClient, cursor string, limit int, currentOnly, allowRestart bool) (api.RoutesPage, bool, error) {
	page, err := client.Routes(ctx, cursor, limit, currentOnly)
	if err == nil {
		return page, false, nil
	}
	var apiErr *api.APIError
	if allowRestart && cursor != "" && errors.As(err, &apiErr) && apiErr.Code == "stale_cursor" {
		_, _ = fmt.Fprintln(a.Err, "warning[stale_cursor]: results changed; restarting from the first page without combining old items")
		page, err = client.Routes(ctx, "", limit, currentOnly)
		if err == nil {
			return page, true, nil
		}
	}
	return api.RoutesPage{}, false, mapAPIError(err)
}

func printRoute(out interface{ Write([]byte) (int, error) }, route api.RouteRevision, current bool, focusStepID string) {
	_, _ = fmt.Fprintf(out, "Route: %s revision=%d current=%t knowledge_revision=%s\n", safeText(route.RouteRevisionID), route.Revision, current, safeText(route.KnowledgeRevisionID))
	for _, step := range route.Steps {
		marker := " "
		if step.RouteStepID == focusStepID {
			marker = ">"
		}
		_, _ = fmt.Fprintf(out, "%s %d node=%s intent=%s completion=%s\n", marker, step.Ordinal, safeText(step.NodeRevisionID), safeText(step.TeachingIntent), safeText(step.CompletionCondition))
	}
}

func (a *App) runEvidence(ctx context.Context, args []string) error {
	set := newFlagSet("evidence")
	var flags onlineFlags
	var limit int
	var cursor, node string
	addOnlineFlags(set, &flags)
	set.IntVar(&limit, "limit", defaultPageLimit, "page size")
	set.StringVar(&cursor, "cursor", "", "opaque page cursor")
	set.StringVar(&node, "node", "", "node revision ID")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "evidence accepts only connection, limit, cursor, and node flags", "run edu-agent evidence", ExitInput)
	}
	if err := validatePageInput(limit, cursor); err != nil {
		return err
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	page, err := a.evidencePage(ctx, online.client, cursor, limit, node)
	if err != nil {
		return err
	}
	printEvidencePage(a.Out, a.Err, page)
	return nil
}

func (a *App) evidencePage(ctx context.Context, client APIClient, cursor string, limit int, node string) (api.EvidencePage, error) {
	page, err := client.Evidence(ctx, cursor, limit, node)
	if err == nil {
		return page, nil
	}
	var apiErr *api.APIError
	if cursor != "" && errors.As(err, &apiErr) && apiErr.Code == "stale_cursor" {
		_, _ = fmt.Fprintln(a.Err, "warning[stale_cursor]: evidence changed; restarting from the first page without combining old items")
		page, err = client.Evidence(ctx, "", limit, node)
		if err == nil {
			return page, nil
		}
	}
	return api.EvidencePage{}, mapAPIError(err)
}

func printEvidencePage(out, errOut interface{ Write([]byte) (int, error) }, page api.EvidencePage) {
	printProjectionWarning(errOut, page.Metadata)
	for _, item := range page.Items {
		_, _ = fmt.Fprintf(out, "Evidence: %s node=%s outcome=%s kind=%s help=%s received=%s\n", safeText(item.EvidenceID), safeText(item.NodeRevisionID), safeText(item.Outcome), safeText(item.Kind), safeText(item.Help), item.ReceivedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if page.NextCursor != "" {
		_, _ = fmt.Fprintf(out, "warning[truncated]: more evidence is available next_cursor=%s\n", safeText(page.NextCursor))
	}
}

func (a *App) runReviews(ctx context.Context, args []string) error {
	set := newFlagSet("reviews")
	var flags onlineFlags
	var limit int
	var cursor, dueBeforeValue string
	addOnlineFlags(set, &flags)
	set.IntVar(&limit, "limit", defaultPageLimit, "page size")
	set.StringVar(&cursor, "cursor", "", "opaque page cursor")
	set.StringVar(&dueBeforeValue, "due-before", "", "RFC3339 upper bound")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "reviews accepts only connection, limit, cursor, and due-before flags", "run edu-agent reviews", ExitInput)
	}
	if err := validatePageInput(limit, cursor); err != nil {
		return err
	}
	dueBefore, err := parseDueBefore(dueBeforeValue)
	if err != nil {
		return err
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	page, err := a.reviewsPage(ctx, online.client, cursor, limit, dueBefore)
	if err != nil {
		return err
	}
	printReviewsPage(a.Out, a.Err, page)
	return nil
}

func (a *App) reviewsPage(ctx context.Context, client APIClient, cursor string, limit int, dueBefore *time.Time) (api.ReviewsPage, error) {
	page, err := client.Reviews(ctx, cursor, limit, dueBefore)
	if err == nil {
		return page, nil
	}
	var apiErr *api.APIError
	if cursor != "" && errors.As(err, &apiErr) && apiErr.Code == "stale_cursor" {
		_, _ = fmt.Fprintln(a.Err, "warning[stale_cursor]: reviews changed; restarting from the first page without combining old items")
		page, err = client.Reviews(ctx, "", limit, dueBefore)
		if err == nil {
			return page, nil
		}
	}
	return api.ReviewsPage{}, mapAPIError(err)
}

func printReviewsPage(out, errOut interface{ Write([]byte) (int, error) }, page api.ReviewsPage) {
	printProjectionWarning(errOut, page.Metadata)
	for _, item := range page.Items {
		_, _ = fmt.Fprintf(out, "Review: node=%s step=%d due=%s policy=%s\n", safeText(item.NodeRevisionID), item.Step, item.DueAt.UTC().Format("2006-01-02T15:04:05Z"), safeText(item.PolicyVersion))
	}
	if page.NextCursor != "" {
		_, _ = fmt.Fprintf(out, "warning[truncated]: more reviews are available next_cursor=%s\n", safeText(page.NextCursor))
	}
}

const progressSnapshotRestartLimit = 1

type progressSnapshot struct {
	status           api.ProjectionStatus
	view             api.SessionView
	active           bool
	nodes            []api.NodeView
	routeMetadata    []api.ProjectionMetadata
	evidenceMetadata []api.ProjectionMetadata
	reviewMetadata   []api.ProjectionMetadata
	truncated        bool
	evidence         api.EvidencePage
	reviews          api.ReviewsPage
}

type progressRouteSnapshot struct {
	nodeIDs       []string
	truncated     bool
	metadata      []api.ProjectionMetadata
	generation    string
	restartReason string
}

func (a *App) runProgress(ctx context.Context, args []string) error {
	set := newFlagSet("progress")
	var flags onlineFlags
	var all bool
	addOnlineFlags(set, &flags)
	set.BoolVar(&all, "all", false, "read bounded additional pages")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "progress accepts only connection flags and --all", "run edu-agent progress", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	snapshot, err := a.progressSnapshot(ctx, online.client, all)
	if err != nil {
		return err
	}
	printProjectionWarning(a.Err, snapshot.status.Metadata)
	_, _ = fmt.Fprintf(a.Out, "Projection: as_of=%d high_water=%d version=%s knowledge_revision=%s\n", snapshot.status.Metadata.AsOfEventSeq, snapshot.status.CommittedEventHighWater, safeText(snapshot.status.Metadata.ProjectionVersion), safeText(snapshot.status.Metadata.KnowledgeRevisionID))
	if snapshot.active {
		printProjectionWarning(a.Err, snapshot.view.Metadata)
		_, _ = fmt.Fprintf(a.Out, "Current: session=%s state=%s active_time=%ds estimated=%t samples=%d\n", safeText(snapshot.view.Session.SessionID), safeText(snapshot.view.Session.State), snapshot.view.EstimatedActiveTime.DurationSeconds, snapshot.view.EstimatedActiveTime.Estimated, snapshot.view.EstimatedActiveTime.SampleCount)
	} else {
		_, _ = fmt.Fprintln(a.Out, "Current: no active session")
	}
	for _, metadata := range snapshot.routeMetadata {
		printProjectionWarning(a.Err, metadata)
	}
	for _, node := range snapshot.nodes {
		printProjectionWarning(a.Err, node.Metadata)
		_, _ = fmt.Fprintf(a.Out, "Node: %s mastery=%s evidence=%d pending_assessments=%d\n", safeText(node.Node.Mastery.NodeRevisionID), safeText(node.Node.Mastery.State), node.Node.Mastery.ValidEvidenceCount, node.Node.Mastery.PendingAssessments)
		for _, misconception := range node.Node.Misconceptions {
			_, _ = fmt.Fprintf(a.Out, "Misconception: %s status=%s candidate=%s\n", safeText(misconception.MisconceptionID), safeText(misconception.Status), safeText(misconception.Candidate))
		}
		if node.Node.Review != nil {
			_, _ = fmt.Fprintf(a.Out, "Review: step=%d due=%s\n", node.Node.Review.Step, node.Node.Review.DueAt.UTC().Format("2006-01-02T15:04:05Z"))
		}
	}
	for _, metadata := range snapshot.evidenceMetadata {
		printProjectionWarning(a.Err, metadata)
	}
	for _, metadata := range snapshot.reviewMetadata {
		printProjectionWarning(a.Err, metadata)
	}
	itemScope := "current-page"
	if all {
		itemScope = "bounded"
	}
	_, _ = fmt.Fprintf(a.Out, "Evidence: %d %s items\nReviews: %d %s items\n", len(snapshot.evidence.Items), itemScope, len(snapshot.reviews.Items), itemScope)
	if snapshot.truncated || snapshot.evidence.NextCursor != "" || snapshot.reviews.NextCursor != "" {
		_, _ = fmt.Fprintln(a.Err, "warning[truncated]: progress is bounded; additional route, evidence, or review pages exist")
	}
	return nil
}

func (a *App) progressSnapshot(ctx context.Context, client APIClient, all bool) (progressSnapshot, error) {
	for attempt := 0; attempt <= progressSnapshotRestartLimit; attempt++ {
		snapshot, restartReason, err := a.progressSnapshotAttempt(ctx, client, all)
		if err != nil {
			return progressSnapshot{}, err
		}
		if restartReason == "" {
			return snapshot, nil
		}
		if attempt == progressSnapshotRestartLimit {
			return progressSnapshot{}, commandError("unstable_progress_snapshot", "projection generation changed while reading the complete progress snapshot", "retry after the projection stabilizes", ExitConflict)
		}
		if restartReason == "stale_cursor" {
			_, _ = fmt.Fprintln(a.Err, "warning[stale_cursor]: results changed; restarting the complete progress snapshot without combining old items")
		} else {
			_, _ = fmt.Fprintln(a.Err, "warning[progress_snapshot]: projection generation changed; restarting the complete snapshot without combining old items")
		}
	}
	return progressSnapshot{}, commandError("unstable_progress_snapshot", "projection generation changed while reading the complete progress snapshot", "retry after the projection stabilizes", ExitConflict)
}

func (a *App) progressSnapshotAttempt(ctx context.Context, client APIClient, all bool) (progressSnapshot, string, error) {
	status, err := client.ProjectionStatus(ctx)
	if err != nil {
		return progressSnapshot{}, "", mapAPIError(err)
	}
	view, active, err := a.currentSession(ctx, client)
	if err != nil {
		return progressSnapshot{}, "", err
	}
	generation := status.Metadata.Generation
	if active && !adoptProgressGeneration(&generation, view.Metadata.Generation) {
		return progressSnapshot{}, "generation_mismatch", nil
	}

	routes := progressRouteSnapshot{nodeIDs: progressBaseNodeIDs(view, active), generation: generation}
	if all || !active {
		routes, err = a.collectProgressRoutes(ctx, client, view, active, all, generation)
		if err != nil {
			return progressSnapshot{}, "", err
		}
		if routes.restartReason != "" {
			return progressSnapshot{}, routes.restartReason, nil
		}
		generation = routes.generation
	}
	nodes := make([]api.NodeView, 0, len(routes.nodeIDs))
	for _, nodeID := range routes.nodeIDs {
		node, nodeErr := client.Node(ctx, nodeID)
		if nodeErr != nil {
			return progressSnapshot{}, "", mapAPIError(nodeErr)
		}
		if !adoptProgressGeneration(&generation, node.Metadata.Generation) {
			return progressSnapshot{}, "generation_mismatch", nil
		}
		nodes = append(nodes, node)
	}
	pageLimit := 1
	if all {
		pageLimit = maxProgressPages
	}
	evidencePages, err := collectProjectionPages(generation, pageLimit, func(cursor string) (api.ProjectionMetadata, []api.AcceptedEvidence, string, error) {
		page, pageErr := client.Evidence(ctx, cursor, defaultPageLimit, "")
		return page.Metadata, page.Items, page.NextCursor, pageErr
	})
	if err != nil {
		return progressSnapshot{}, "", err
	}
	if evidencePages.restartReason != "" {
		return progressSnapshot{}, evidencePages.restartReason, nil
	}
	generation = evidencePages.generation
	reviewPages, err := collectProjectionPages(generation, pageLimit, func(cursor string) (api.ProjectionMetadata, []api.ReviewSchedule, string, error) {
		page, pageErr := client.Reviews(ctx, cursor, defaultPageLimit, nil)
		return page.Metadata, page.Items, page.NextCursor, pageErr
	})
	if err != nil {
		return progressSnapshot{}, "", err
	}
	if reviewPages.restartReason != "" {
		return progressSnapshot{}, reviewPages.restartReason, nil
	}
	evidence := api.EvidencePage{Items: evidencePages.items, NextCursor: evidencePages.nextCursor}
	if len(evidencePages.metadata) != 0 {
		evidence.Metadata = evidencePages.metadata[0]
	}
	reviews := api.ReviewsPage{Items: reviewPages.items, NextCursor: reviewPages.nextCursor}
	if len(reviewPages.metadata) != 0 {
		reviews.Metadata = reviewPages.metadata[0]
	}
	return progressSnapshot{
		status: status, view: view, active: active, nodes: nodes,
		routeMetadata: routes.metadata, evidenceMetadata: evidencePages.metadata, reviewMetadata: reviewPages.metadata,
		truncated: routes.truncated || evidencePages.truncated || reviewPages.truncated, evidence: evidence, reviews: reviews,
	}, "", nil
}

func adoptProgressGeneration(expected *string, observed string) bool {
	if *expected == "" {
		*expected = observed
		return true
	}
	return *expected == observed
}

type boundedPageSnapshot[T any] struct {
	items         []T
	metadata      []api.ProjectionMetadata
	nextCursor    string
	truncated     bool
	generation    string
	restartReason string
}

func collectProjectionPages[T any](expectedGeneration string, maxPages int, fetch func(string) (api.ProjectionMetadata, []T, string, error)) (boundedPageSnapshot[T], error) {
	result := boundedPageSnapshot[T]{generation: expectedGeneration}
	if maxPages < 1 {
		maxPages = 1
	}
	cursor := ""
	for pageIndex := 0; pageIndex < maxPages; pageIndex++ {
		metadata, items, nextCursor, err := fetch(cursor)
		if err != nil {
			var apiErr *api.APIError
			if cursor != "" && errors.As(err, &apiErr) && apiErr.Code == "stale_cursor" {
				result.restartReason = "stale_cursor"
				return result, nil
			}
			return boundedPageSnapshot[T]{}, mapAPIError(err)
		}
		if !adoptProgressGeneration(&result.generation, metadata.Generation) {
			result.restartReason = "generation_mismatch"
			return result, nil
		}
		result.metadata = append(result.metadata, metadata)
		result.items = append(result.items, items...)
		result.nextCursor = nextCursor
		result.truncated = nextCursor != ""
		if nextCursor == "" {
			return result, nil
		}
		cursor = nextCursor
	}
	return result, nil
}

func progressBaseNodeIDs(view api.SessionView, active bool) []string {
	if !active || view.WorkItem == nil || view.WorkItem.RouteRevision == nil {
		return nil
	}
	ids := make([]string, 0, len(view.WorkItem.RouteRevision.Steps))
	seen := make(map[string]bool, len(view.WorkItem.RouteRevision.Steps))
	for _, step := range view.WorkItem.RouteRevision.Steps {
		if !seen[step.NodeRevisionID] {
			seen[step.NodeRevisionID] = true
			ids = append(ids, step.NodeRevisionID)
		}
	}
	return ids
}

func (a *App) collectProgressRoutes(ctx context.Context, client APIClient, view api.SessionView, active, all bool, expectedGeneration string) (progressRouteSnapshot, error) {
	result := progressRouteSnapshot{nodeIDs: progressBaseNodeIDs(view, active), generation: expectedGeneration}
	if active && !all {
		return result, nil
	}
	seen := make(map[string]bool, len(result.nodeIDs))
	for _, nodeID := range result.nodeIDs {
		seen[nodeID] = true
	}
	cursor := ""
	pages := 1
	if all {
		pages = maxProgressPages
	}
	for pageIndex := 0; pageIndex < pages; pageIndex++ {
		page, err := client.Routes(ctx, cursor, defaultPageLimit, true)
		if err != nil {
			var apiErr *api.APIError
			if cursor != "" && errors.As(err, &apiErr) && apiErr.Code == "stale_cursor" {
				result.restartReason = "stale_cursor"
				return result, nil
			}
			return progressRouteSnapshot{}, mapAPIError(err)
		}
		if !adoptProgressGeneration(&result.generation, page.Metadata.Generation) {
			result.restartReason = "generation_mismatch"
			return result, nil
		}
		result.metadata = append(result.metadata, page.Metadata)
		for _, route := range page.Items {
			for _, step := range route.Route.Steps {
				if !seen[step.NodeRevisionID] {
					if len(result.nodeIDs) >= maxProgressNodes {
						result.truncated = true
						sort.Strings(result.nodeIDs)
						return result, nil
					}
					seen[step.NodeRevisionID] = true
					result.nodeIDs = append(result.nodeIDs, step.NodeRevisionID)
				}
			}
		}
		if page.NextCursor == "" {
			result.truncated = false
			sort.Strings(result.nodeIDs)
			return result, nil
		}
		cursor = page.NextCursor
		result.truncated = true
	}
	sort.Strings(result.nodeIDs)
	return result, nil
}

func (a *App) progressNodeIDs(ctx context.Context, client APIClient, view api.SessionView, active, all bool) ([]string, bool, error) {
	for attempt := 0; attempt <= progressSnapshotRestartLimit; attempt++ {
		result, err := a.collectProgressRoutes(ctx, client, view, active, all, view.Metadata.Generation)
		if err != nil {
			return nil, false, err
		}
		if result.restartReason == "" {
			return result.nodeIDs, result.truncated, nil
		}
		if !all {
			return result.nodeIDs, result.truncated, nil
		}
		if attempt == progressSnapshotRestartLimit {
			return nil, false, commandError("unstable_progress_snapshot", "projection generation changed while reading the complete progress snapshot", "retry after the projection stabilizes", ExitConflict)
		}
		view, active, err = a.currentSession(ctx, client)
		if err != nil {
			return nil, false, err
		}
		if result.restartReason == "stale_cursor" {
			_, _ = fmt.Fprintln(a.Err, "warning[stale_cursor]: results changed; restarting from the first page without combining old items")
		} else {
			_, _ = fmt.Fprintln(a.Err, "warning[progress_snapshot]: projection generation changed; restarting from the first page without combining old items")
		}
	}
	return nil, false, commandError("unstable_progress_snapshot", "projection generation changed while reading the complete progress snapshot", "retry after the projection stabilizes", ExitConflict)
}
