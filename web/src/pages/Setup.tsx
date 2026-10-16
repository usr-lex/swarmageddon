import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { api } from "@/api";
import { Button, Card, ErrorText, Field, Input } from "@/components/ui";

/** Claiming admin with the setup token (§3.3). */
export function SetupPage() {
  const setup = useQuery({ queryKey: ["setup"], queryFn: () => api().getSetupState() });
  const [claim, setClaim] = useState<string>();
  return (
    <div className="mx-auto max-w-lg space-y-4">
      <h1 className="text-2xl font-semibold">Set up swarmageddon</h1>
      {setup.data && (
        <Card title="Check you reached the right server">
          <p className="text-sm text-muted">
            The install printed the CA fingerprint. If your browser shows the same fingerprint for this site's certificate authority,
            nobody is in between.
          </p>
          <p className="mt-2 font-mono text-xs break-all" data-testid="ca-fingerprint">
            {setup.data.caFingerprint}
          </p>
        </Card>
      )}
      {claim ? <CreateAdmin claim={claim} /> : <TokenForm onClaim={setClaim} />}
    </div>
  );
}

function TokenForm({ onClaim }: { onClaim: (c: string) => void }) {
  const [token, setToken] = useState("");
  const m = useMutation({ mutationFn: () => api().claimSetup(token), onSuccess: (r) => onClaim(r.claim) });
  return (
    <Card title="1. Setup token">
      <form
        className="space-y-3"
        onSubmit={(e) => {
          e.preventDefault();
          m.mutate();
        }}
      >
        <Field
          label="Setup token"
          hint={
            <>
              Printed by <code>kubectl -n swarmageddon-system logs job/welcome</code>, or read it with{" "}
              <code>kubectl -n swarmageddon-system get secret setup-token -o jsonpath='{"{.data.token}"}' | base64 -d</code>.
            </>
          }
        >
          {(id, d) => (
            <Input
              id={id}
              aria-describedby={d}
              value={token}
              onChange={(e) => setToken(e.target.value)}
              autoComplete="off"
              spellCheck={false}
              required
              className="font-mono"
            />
          )}
        </Field>
        <ErrorText error={m.error} />
        <Button type="submit" variant="primary" busy={m.isPending} disabled={!token.trim()}>
          Continue
        </Button>
      </form>
    </Card>
  );
}

function CreateAdmin({ claim }: { claim: string }) {
  const [username, setUsername] = useState("admin");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const qc = useQueryClient();
  const navigate = useNavigate();
  const done = async () => {
    await qc.invalidateQueries();
    await navigate({ to: "/wizard" });
  };
  const create = useMutation({ mutationFn: () => api().createLocalAdmin(claim, { username, email, password }), onSuccess: done });
  const idp = useMutation({ mutationFn: () => api().continueWithIdP(claim), onSuccess: done });
  const mismatch = confirm.length > 0 && confirm !== password;
  return (
    <Card title="2. Create the first admin">
      <form
        className="space-y-3"
        onSubmit={(e) => {
          e.preventDefault();
          if (!mismatch) create.mutate();
        }}
      >
        <Field label="Username">{(id) => <Input id={id} value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" required />}</Field>
        <Field label="Email">{(id) => <Input id={id} type="email" value={email} onChange={(e) => setEmail(e.target.value)} autoComplete="email" required />}</Field>
        <Field label="Password" hint="At least 12 characters.">
          {(id, d) => (
            <Input id={id} aria-describedby={d} type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" minLength={12} required />
          )}
        </Field>
        <Field label="Confirm password" error={mismatch ? "The passwords differ." : undefined}>
          {(id, d) => (
            <Input id={id} aria-describedby={d} aria-invalid={mismatch} type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="new-password" required />
          )}
        </Field>
        <ErrorText error={create.error ?? idp.error} />
        <div className="flex flex-wrap gap-2">
          <Button type="submit" variant="primary" busy={create.isPending} disabled={mismatch}>
            Create admin
          </Button>
          <Button onClick={() => idp.mutate()} busy={idp.isPending}>
            Skip: set up OIDC or SAML first
          </Button>
        </div>
        <p className="text-xs text-muted">
          Skipping keeps this browser signed in for setup only; the first person mapped to the admin role by your identity provider becomes admin.
        </p>
      </form>
    </Card>
  );
}
