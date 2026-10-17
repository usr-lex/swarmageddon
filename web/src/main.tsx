import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import "./index.css";
import { apiMode, initApi } from "./api";
import { initSentry, reportCaughtError } from "./lib/sentry";
import { applyTheme, initialTheme } from "./lib/theme";
import { queryClient, router } from "./router";

applyTheme(initialTheme());

if ("serviceWorker" in navigator && import.meta.env.PROD) {
  navigator.serviceWorker.register("/sw.js").catch(() => {
    /* push and install stay unavailable */
  });
}

// Error reporting, if the control center has it on; it loads alongside the app.
if (apiMode() === "real") void initSentry(router);

await initApi();

createRoot(document.getElementById("root")!, { onCaughtError: reportCaughtError }).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
);
