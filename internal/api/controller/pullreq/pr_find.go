// Copyright 2022 Harness Inc. All rights reserved.
// Use of this source code is governed by the Polyform Free Trial License
// that can be found in the LICENSE.md file for this repository.

package pullreq

import (
	"context"
	"fmt"

	"github.com/harness/gitness/gitrpc"
	"github.com/harness/gitness/internal/api/usererror"
	"github.com/harness/gitness/internal/auth"
	"github.com/harness/gitness/types"
	"github.com/harness/gitness/types/enum"
)

// Find returns a pull request from the provided repository.
func (c *Controller) Find(
	ctx context.Context,
	session *auth.Session,
	repoRef string,
	pullreqNum int64,
) (*types.PullReq, error) {
	if pullreqNum <= 0 {
		return nil, usererror.BadRequest("A valid pull request number must be provided.")
	}

	repo, err := c.getRepoCheckAccess(ctx, session, repoRef, enum.PermissionRepoView)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire access to the repo: %w", err)
	}

	pr, err := c.pullreqStore.FindByNumber(ctx, repo.ID, pullreqNum)
	if err != nil {
		return nil, err
	}

	headRef := pr.SourceSHA
	baseRef := pr.MergeBaseSHA

	output, err := c.gitRPCClient.DiffStats(ctx, &gitrpc.DiffParams{
		ReadParams: gitrpc.CreateRPCReadParams(repo),
		BaseRef:    baseRef,
		HeadRef:    headRef,
	})
	if err != nil {
		return nil, err
	}

	pr.Stats.DiffStats.Commits = output.Commits
	pr.Stats.DiffStats.FilesChanged = output.FilesChanged

	return pr, nil
}
