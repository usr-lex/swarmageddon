// The parts of the Sentry SDK the UI uses, so that lazily importing this
// module bundles only them: importing "@sentry/react" itself dynamically
// would keep everything it exports, session replay included.
export { init, reactErrorHandler, tanstackRouterBrowserTracingIntegration } from "@sentry/react";
