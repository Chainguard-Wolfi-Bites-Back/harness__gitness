/*
 * Copyright 2023 Harness, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

import React, { useEffect, useMemo, useState } from 'react'
import { random } from 'lodash-es'
import { useMutate } from 'restful-react'
import { useToaster, Button, ButtonVariation, ButtonSize, useIsMounted } from '@harnessio/uicore'
import { useStrings } from 'framework/strings'
import type { GitInfoProps } from 'utils/GitUtils'
import type { TypesPullReqActivity } from 'services/code'
import { useEmitCodeCommentStatus } from 'hooks/useEmitCodeCommentStatus'
import { CodeCommentState, getErrorMessage } from 'utils/Utils'
import type { CommentItem } from '../CommentBox/CommentBox'

interface CodeCommentStatusButtonProps extends Pick<GitInfoProps, 'repoMetadata' | 'pullReqMetadata'> {
  commentItems: CommentItem<TypesPullReqActivity>[]
}

export const CodeCommentStatusButton: React.FC<CodeCommentStatusButtonProps> = ({
  repoMetadata,
  pullReqMetadata,
  commentItems
}) => {
  const isMounted = useIsMounted()
  const { getString } = useStrings()
  const { showError } = useToaster()
  const path = useMemo(
    () => `/api/v1/repos/${repoMetadata.path}/+/pullreq/${pullReqMetadata?.number}/comments`,
    [repoMetadata.path, pullReqMetadata?.number]
  )
  const { mutate: updateCodeCommentStatus } = useMutate({ verb: 'PUT', path: ({ id }) => `${path}/${id}/status` })
  const [parentComment, setParentComment] = useState(commentItems[0])
  const randomClass = useMemo(() => `CodeCommentStatusButton-${random(1_000_000, false)}`, [])
  const [resolved, setResolved] = useState(parentComment?.payload?.resolved ? true : false)
  const emitCodeCommentStatus = useEmitCodeCommentStatus({
    id: parentComment?.id,
    onMatch: status => {
      if (isMounted.current) {
        const isResolved = status === CodeCommentState.RESOLVED
        setResolved(isResolved)

        if (parentComment?.payload) {
          if (isResolved) {
            parentComment.payload.resolved = Date.now()
          } else {
            parentComment.payload.resolved = 0
          }
        }
      }
    }
  })

  useEffect(() => {
    // Comment thread has been just created, check if parentComment is
    // not set up properly, then query the comment id from DOM and construct
    // it from scratch (this is a workaround).
    if (!parentComment?.id) {
      const id = document
        .querySelector(`.${randomClass}`)
        ?.closest('[data-comment-thread-id]')
        ?.getAttribute('data-comment-thread-id')

      if (id) {
        setParentComment({
          id: Number(id),
          payload: { resolved: 0 }
        } as CommentItem<TypesPullReqActivity>)
      }
    }
  }, [parentComment?.id])

  return parentComment?.deleted ? null : (
    <Button
      className={randomClass}
      text={getString(resolved ? 'reactivate' : 'resolve')}
      variation={ButtonVariation.TERTIARY}
      size={ButtonSize.MEDIUM}
      onClick={async () => {
        const status = resolved ? CodeCommentState.ACTIVE : CodeCommentState.RESOLVED
        const payload = { status }
        const id = parentComment?.id

        updateCodeCommentStatus(payload, { pathParams: { id } })
          .then(() => {
            emitCodeCommentStatus(status)

            if (isMounted.current) {
              setResolved(!resolved)
            }
          })
          .catch(_exception => {
            showError(getErrorMessage(_exception), 0, getString('pr.failedToUpdateCommentStatus'))
          })
      }}></Button>
  )
}
