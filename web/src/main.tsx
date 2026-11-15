import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import "./styles.css";
import { createQueryClient } from "./api/queryClient";
import { Root } from "./app/Root";
import { createAppRouter } from "./app/router";
import { boot, browserBootDeps, signIn } from "./auth/boot";
import { setSession } from "./auth/session";
import { SigningIn } from "./screens/session";

const el = document.getElementById("root");
if (!el) throw new Error("index.html has no #root");
const root = createRoot(el);
root.render(<SigningIn />);

// Once per page load, outside React: the callback's code is single-use.
void boot().then((b) => {
  if (b.kind === "session") setSession(b.session);
  root.render(
    <StrictMode>
      <Root
        boot={b}
        queryClient={createQueryClient()}
        createRouter={() => createAppRouter()}
        signIn={(provider, returnTo) => void signIn(provider, returnTo, browserBootDeps())}
      />
    </StrictMode>,
  );
});
