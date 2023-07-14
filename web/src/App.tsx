import React, { useEffect, useState, useCallback, useMemo } from 'react'
import { RestfulProvider } from 'restful-react'
import cx from 'classnames'
import { Container } from '@harness/uicore'
import { ModalProvider } from '@harness/use-modal'
import { FocusStyleManager } from '@blueprintjs/core'
import AppErrorBoundary from 'framework/AppErrorBoundary/AppErrorBoundary'
import { AppContextProvider, defaultCurrentUser } from 'AppContext'
import type { AppProps } from 'AppProps'
import { buildResfulReactRequestOptions, handle401 } from 'AppUtils'
import { RouteDestinations } from 'RouteDestinations'
import { useAPIToken } from 'hooks/useAPIToken'
import { routes as _routes } from 'RouteDefinitions'
import { getConfig } from 'services/config'
import { languageLoader } from './framework/strings/languageLoader'
import type { LanguageRecord } from './framework/strings/languageLoader'
import { StringsContextProvider } from './framework/strings/StringsContextProvider'
import 'highlight.js/styles/github.css'
import 'diff2html/bundles/css/diff2html.min.css'
import css from './App.module.scss'

FocusStyleManager.onlyShowFocusOnTabs()

const App: React.FC<AppProps> = React.memo(function App({
  standalone = false,
  space = '',
  routes = _routes,
  lang = 'en',
  on401 = handle401,
  children,
  hooks,
  currentUserProfileURL = ''
}: AppProps) {
  const [strings, setStrings] = useState<LanguageRecord>()
  const [token] = useAPIToken()
  const getRequestOptions = useCallback(
    (): Partial<RequestInit> => buildResfulReactRequestOptions(hooks?.useGetToken?.() || token),
    [token, hooks]
  )
  const routingId = useMemo(() => (standalone ? '' : space.split('/').shift() || ''), [standalone, space])
  const queryParams = useMemo(() => (!standalone ? { routingId } : {}), [standalone, routingId])

  useEffect(() => {
    languageLoader(lang).then(setStrings)
  }, [lang, setStrings])

  const Wrapper: React.FC<{ fullPage: boolean }> = useCallback(
    props => {
      return strings ? (
        <Container className={cx(css.main, { [css.fullPage]: standalone && props.fullPage })}>
          <StringsContextProvider initialStrings={strings}>
            <AppErrorBoundary>
              <RestfulProvider
                base={standalone ? '/' : getConfig('code')}
                requestOptions={getRequestOptions}
                queryParams={queryParams}
                queryParamStringifyOptions={{ skipNulls: true }}
                onResponse={response => {
                  if (!response.ok && response.status === 401) {
                    on401()
                  }
                }}>
                <AppContextProvider
                  value={{
                    standalone,
                    routingId,
                    space,
                    routes,
                    lang,
                    on401,
                    hooks,
                    currentUser: defaultCurrentUser,
                    currentUserProfileURL
                  }}>
                  <ModalProvider>{props.children ? props.children : <RouteDestinations />}</ModalProvider>
                </AppContextProvider>
              </RestfulProvider>
            </AppErrorBoundary>
          </StringsContextProvider>
        </Container>
      ) : null
    },
    [strings] // eslint-disable-line react-hooks/exhaustive-deps
  )

  useEffect(() => {
    AppWrapper = function _AppWrapper({ children: _children }) {
      return <Wrapper fullPage={false}>{_children}</Wrapper>
    }
  }, [Wrapper])

  return <Wrapper fullPage>{children}</Wrapper>
})

export let AppWrapper: React.FC = () => <Container />
export default App
