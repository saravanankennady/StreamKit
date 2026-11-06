"use client";

import Link from "next/link";
import { useSearchParams } from "next/navigation";
import * as React from "react";
import { useForm } from "react-hook-form";
import { AuthHeading, useRedirectIfSignedIn } from "@/components/shell/auth-frame";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { api } from "@/lib/api";
import { applyApiError } from "@/lib/form";

/** Only same-site paths, so a crafted link cannot send someone elsewhere after they sign in. */
function safeNext(next: string | null) {
  return next && next.startsWith("/") && !next.startsWith("//") ? next : "/overview";
}

export default function LoginPage() {
  useRedirectIfSignedIn();
  const params = useSearchParams();
  const form = useForm({ defaultValues: { email: "", password: "" } });
  const [error, setError] = React.useState<string | null>(null);

  const submit = form.handleSubmit(async (values) => {
    setError(null);
    try {
      await api("/auth/login", { method: "POST", body: values });
      window.location.assign(safeNext(params.get("next")));
    } catch (err) {
      setError(applyApiError(form, err));
    }
  });

  const { errors, isSubmitting } = form.formState;
  return (
    <>
      <AuthHeading title="Sign in" />
      <form onSubmit={submit} className="flex flex-col gap-3" noValidate>
        <Field label="Email" error={errors.email?.message}>
          <Input type="email" autoComplete="email" autoFocus {...form.register("email", { required: "Enter your email address." })} />
        </Field>
        <Field label="Password" error={errors.password?.message}>
          <Input type="password" autoComplete="current-password" {...form.register("password", { required: "Enter your password." })} />
        </Field>
        {error && (
          <p role="alert" className="text-sm text-critical-text">
            {error}
          </p>
        )}
        <Button type="submit" variant="primary" size="lg" pending={isSubmitting}>
          Sign in
        </Button>
      </form>
      <div className="mt-5 flex flex-col gap-1.5 text-sm text-ink-2">
        <Link className="link w-fit" href="/forgot-password">
          Forgot your password?
        </Link>
        <span>
          New here?{" "}
          <Link className="link" href="/register">
            Create an account
          </Link>
        </span>
      </div>
    </>
  );
}
