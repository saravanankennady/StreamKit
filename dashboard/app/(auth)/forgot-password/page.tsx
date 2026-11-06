"use client";

import Link from "next/link";
import * as React from "react";
import { useForm } from "react-hook-form";
import { AuthHeading } from "@/components/shell/auth-frame";
import { Callout } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Field } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { api } from "@/lib/api";
import { applyApiError } from "@/lib/form";

export default function ForgotPasswordPage() {
  const form = useForm({ defaultValues: { email: "" } });
  const [error, setError] = React.useState<string | null>(null);
  const [sentTo, setSentTo] = React.useState<string | null>(null);

  const submit = form.handleSubmit(async (values) => {
    setError(null);
    try {
      await api("/auth/forgot-password", { method: "POST", body: values });
      setSentTo(values.email);
    } catch (err) {
      setError(applyApiError(form, err));
    }
  });

  const { errors, isSubmitting } = form.formState;
  return (
    <>
      <AuthHeading title="Reset your password">We will email you a link. It works once and expires in an hour.</AuthHeading>
      {sentTo ? (
        <Callout tone="good" title="Check your inbox">
          If {sentTo} has an account, a reset link is on its way. If nothing arrives, check spam, and check the address.
        </Callout>
      ) : (
        <form onSubmit={submit} className="flex flex-col gap-3" noValidate>
          <Field label="Email" error={errors.email?.message}>
            <Input type="email" autoComplete="email" autoFocus {...form.register("email", { required: "Enter your email address." })} />
          </Field>
          {error && (
            <p role="alert" className="text-sm text-critical-text">
              {error}
            </p>
          )}
          <Button type="submit" variant="primary" size="lg" pending={isSubmitting}>
            Send reset link
          </Button>
        </form>
      )}
      <p className="mt-5 text-sm">
        <Link className="link" href="/login">
          Back to sign in
        </Link>
      </p>
    </>
  );
}
