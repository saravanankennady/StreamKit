"use client";

import Link from "next/link";
import * as React from "react";
import { useForm } from "react-hook-form";
import { AuthHeading, useRedirectIfSignedIn } from "@/components/shell/auth-frame";
import { Button } from "@/components/ui/button";
import { Callout } from "@/components/ui/badge";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { api } from "@/lib/api";
import { applyApiError } from "@/lib/form";

export default function RegisterPage() {
  useRedirectIfSignedIn();
  const form = useForm({ defaultValues: { name: "", email: "", organization: "", password: "" } });
  const [error, setError] = React.useState<string | null>(null);
  const [awaitingVerification, setAwaitingVerification] = React.useState<string | null>(null);

  const submit = form.handleSubmit(async (values) => {
    setError(null);
    try {
      const res = await api<{ verification_required?: boolean }>("/auth/register", { method: "POST", body: values });
      if (res.verification_required) setAwaitingVerification(values.email);
      else window.location.assign("/overview");
    } catch (err) {
      setError(applyApiError(form, err));
    }
  });

  if (awaitingVerification) {
    return (
      <>
        <AuthHeading title="Check your inbox" />
        <Callout tone="info" title={`We sent a link to ${awaitingVerification}`}>
          This server needs your address confirmed before you can sign in. The link works once and expires in 24 hours.
        </Callout>
        <p className="mt-4 text-sm">
          <Link className="link" href="/login">
            Back to sign in
          </Link>
        </p>
      </>
    );
  }

  const { errors, isSubmitting } = form.formState;
  return (
    <>
      <AuthHeading title="Create your account">The first account on a server also becomes its platform administrator.</AuthHeading>
      <form onSubmit={submit} className="flex flex-col gap-3" noValidate>
        <Field label="Your name" error={errors.name?.message}>
          <Input autoComplete="name" autoFocus {...form.register("name", { required: "Enter your name." })} />
        </Field>
        <Field label="Email" error={errors.email?.message}>
          <Input type="email" autoComplete="email" {...form.register("email", { required: "Enter your email address." })} />
        </Field>
        <Field label="Organization" hint="Your team or company. Projects live inside it." error={errors.organization?.message}>
          <Input autoComplete="organization" {...form.register("organization", { required: "Enter a name for your organization." })} />
        </Field>
        <Field label="Password" hint="At least 10 characters." error={errors.password?.message}>
          <Input type="password" autoComplete="new-password" {...form.register("password", { required: "Choose a password.", minLength: { value: 10, message: "Use at least 10 characters." } })} />
        </Field>
        {error && (
          <p role="alert" className="text-sm text-critical-text">
            {error}
          </p>
        )}
        <Button type="submit" variant="primary" size="lg" pending={isSubmitting}>
          Create account
        </Button>
      </form>
      <p className="mt-5 text-sm text-ink-2">
        Already have an account?{" "}
        <Link className="link" href="/login">
          Sign in
        </Link>
      </p>
    </>
  );
}
