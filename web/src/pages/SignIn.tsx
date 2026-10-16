import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate, useSearch } from "@tanstack/react-router";
import { useState } from "react";
import { api } from "@/api";
import { Button, Card, ErrorText, Field, Input } from "@/components/ui";
import { providerLoginURL } from "@/lib/auth";

export function SignInPage() {
  const { next, error } = useSearch({ from: "/signin" });
  const providers = useQuery({ queryKey: ["providers"], queryFn: () => api().listAuthProviders() });
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const qc = useQueryClient();
  const navigate = useNavigate();
  const login = useMutation({
    mutationFn: () => api().loginLocal(username, password),
    onSuccess: async () => {
      await qc.invalidateQueries();
      await navigate({ to: safeNext(next) });
    },
  });
  return (
    <div className="mx-auto max-w-sm space-y-4">
      <h1 className="text-2xl font-semibold">Sign in</h1>
      {error && <ErrorText error={error} />}
      {!!providers.data?.length && (
        <Card>
          <ul className="flex flex-col gap-2">
            {providers.data.map((p) => (
              <li key={`${p.kind}/${p.name}`}>
                <a
                  href={providerLoginURL(p, safeNext(next))}
                  className="flex min-h-10 w-full items-center justify-center rounded-md border border-border bg-surface-2 px-3 text-sm font-medium hover:bg-border/50"
                >
                  Continue with {p.displayName}
                </a>
              </li>
            ))}
          </ul>
        </Card>
      )}
      <Card title="Local account">
        <form
          className="space-y-3"
          onSubmit={(e) => {
            e.preventDefault();
            login.mutate();
          }}
        >
          <Field label="Username">{(id) => <Input id={id} value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" required />}</Field>
          <Field label="Password">
            {(id) => <Input id={id} type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required />}
          </Field>
          <ErrorText error={login.error} />
          <Button type="submit" variant="primary" busy={login.isPending} className="w-full">
            Sign in
          </Button>
        </form>
      </Card>
    </div>
  );
}

/** Only same-origin paths are followed after sign-in. */
export function safeNext(next?: string): string {
  return next && next.startsWith("/") && !next.startsWith("//") && next !== "/signin" ? next : "/";
}
