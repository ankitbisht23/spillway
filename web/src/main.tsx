import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import "./styles.css";
import { createQueryClient } from "./api/queryClient";
import { App } from "./app/App";
import { createAppRouter } from "./app/router";

const root = document.getElementById("root");
if (!root) throw new Error("index.html has no #root");
createRoot(root).render(
  <StrictMode>
    <App router={createAppRouter()} queryClient={createQueryClient()} />
  </StrictMode>,
);
