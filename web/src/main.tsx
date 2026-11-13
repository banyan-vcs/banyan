import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { BASE_URL } from "./api/client";
import { AppRoutes } from "./App";
import { ModeProvider } from "./mode";
import { SessionProvider } from "./session";
import "./styles.css";

const queryClient = new QueryClient({
  defaultOptions: { queries: { refetchOnWindowFocus: false } },
});

const basename = BASE_URL.replace(/\/$/, "");

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ModeProvider>
        <SessionProvider>
          <BrowserRouter basename={basename}>
            <AppRoutes />
          </BrowserRouter>
        </SessionProvider>
      </ModeProvider>
    </QueryClientProvider>
  </StrictMode>
);
