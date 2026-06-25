// LoginPage shows OAuth provider buttons when auth is enabled and user is not logged in.

import { For } from "solid-js";

import { useAuth } from "../AuthContext";
import GitHubIcon from "../components/github.svg?solid";
import GitLabIcon from "../components/gitlab.svg?solid";
import GoogleIcon from "../components/google.svg?solid";

function providerIcon(provider: string) {
  if (provider === "github") return <GitHubIcon width="1.4em" height="1.4em" />;
  if (provider === "gitlab") return <GitLabIcon width="1.4em" height="1.4em" />;
  if (provider === "google") return <GoogleIcon width="1.4em" height="1.4em" />;
  return null;
}

function providerLabel(provider: string): string {
  if (provider === "github") return "Sign in with GitHub";
  if (provider === "gitlab") return "Sign in with GitLab";
  if (provider === "google") return "Sign in with Google";
  return `Sign in with ${provider}`;
}

export default function LoginPage() {
  const { providers } = useAuth();

  return (
    <div class="login-page">
      <div class="login-card">
        <h1 class="login-title">caic</h1>
        <p class="login-subtitle">Coding Agents in Containers</p>
        <div class="login-buttons">
          <For each={providers()}>
            {(provider) => (
              <a href={`/auth/${provider}/start`} class="login-button">
                {providerIcon(provider)}
                {providerLabel(provider)}
              </a>
            )}
          </For>
        </div>
      </div>
    </div>
  );
}
