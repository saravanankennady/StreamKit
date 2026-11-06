"use client";

import Link from "next/link";
import { useSearchParams } from "next/navigation";
import * as React from "react";
import { useForm } from "react-hook-form";
import { AuthHeading } from "@/components/shell/auth-frame";
import { Callout } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { api, ApiError } from "@/lib/api";
import { applyApiError } from "@/lib/form";

export default function ResetPasswordPage() {
  const token = useSearchParams().get("token") ?? "";
  const form = useForm({ defaultValues: { password: "", confirm: "" } });
  const [error, setError] = React.useState<string | null>(null);
  const [expired, setExpired] = React.useState(false);
  const [done, setDone] = React.useState(false);

  const submit = form.handleSubmit(async ({ password }) => {
    setError(null);
    try {
      await api("/auth/reset-password", { method: "POST", body: { token, password } });
      setDone(true);
    } catch (err) {
      if (err instanceof ApiError && err.code === "invalid_token") setExpired(true);
      setError(applyApiError(form, err));
    }
  });

  if (!token) {
    return (
      <>
        <AuthHeading title="This link is incomplete" />
        <p className="text-ink-2">Open the link from your email again, or request a new one.</p>
        <p className="mt-4 text-sm">
          <Link className="link" href="/forgot-password">
            Request a new link
          </Link>
        </p>
      </>
    );
  }

  if (done) {
    return (
      <>
        <AuthHeading title="Password changed" />
        <Callout tone="good" title="You are signed out everywhere">
          All your sessions ended when the password changed. Sign in with the new one.
        </Callout>
        <Button asChild variant="primary" size="lg" className="mt-4 w-full">
          <Link href="/login">Sign in</Link>
        </Button>
      </>
    );
  }

  const { errors, isSubmitting } = form.formState;
  return (
    <>
      <AuthHeading title="Choose a new password">Changing it signs you out of every session.</AuthHeading>
      <form onSubmit={submit} className="flex flex-col gap-3" noValidate>
        <Field label="New password" hint="At least 10 characters." error={errors.password?.message}>
          <Input type="password" autoComplete="new-password" autoFocus {...form.register("password", { required: "Choose a password.", minLength: { value: 10, message: "Use at least 10 characters." } })} />
        </Field>
        <Field label="Type it again" error={errors.confirm?.message}>
          <Input type="password" autoComplete="new-password" {...form.register("confirm", { validate: (v, all) => v === all.password || "The two passwords do not match." })} />
        </Field>
        {error && (
          <p role="alert" className="text-sm text-critical-text">
            {error}
          </p>
        )}
        <Button type="submit" variant="primary" size="lg" pending={isSubmitting}>
          Change password
        </Button>
      </form>
      {expired && (
        <p className="mt-4 text-sm">
          <Link className="link" href="/forgot-password">
            Request a new link
          </Link>
        </p>
      )}
    </>
  );
}
