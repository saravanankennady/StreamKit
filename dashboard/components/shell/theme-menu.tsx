"use client";

import { Check, Moon, Sun, SunMoon } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Dropdown, DropdownContent, DropdownItem, DropdownTrigger } from "@/components/ui/dropdown";
import { useTheme, type ThemeChoice } from "@/lib/theme";

const choices: { value: ThemeChoice; label: string; icon: typeof Sun }[] = [
  { value: "light", label: "Light", icon: Sun },
  { value: "dark", label: "Dark", icon: Moon },
  { value: "system", label: "Match system", icon: SunMoon },
];

export function ThemeMenu() {
  const { choice, setChoice } = useTheme();
  return (
    <Dropdown>
      <DropdownTrigger asChild>
        <Button variant="ghost" size="icon" aria-label="Theme">
          <Sun className="[[data-theme=dark]_&]:hidden" aria-hidden />
          <Moon className="hidden [[data-theme=dark]_&]:block" aria-hidden />
        </Button>
      </DropdownTrigger>
      <DropdownContent>
        {choices.map((c) => (
          <DropdownItem key={c.value} onSelect={() => setChoice(c.value)}>
            <c.icon aria-hidden />
            <span className="flex-1">{c.label}</span>
            {choice === c.value && <Check className="!text-ink" strokeWidth={3} aria-label="Selected" />}
          </DropdownItem>
        ))}
      </DropdownContent>
    </Dropdown>
  );
}
