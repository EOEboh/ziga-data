import { ButtonHTMLAttributes } from "react";

// The three button styles of the old .btn / .btn--primary / .btn--ghost
// classes. Exactly one primary-styled button is on screen at a time.
const VARIANTS = {
  default: "font-medium border-line bg-surface text-text hover:border-text-2",
  primary: "font-semibold border-green bg-green text-white hover:bg-green-deep hover:border-green-deep",
  ghost: "font-medium border-transparent bg-transparent text-text-2 hover:text-text hover:bg-bg",
} as const;

type Props = { variant?: keyof typeof VARIANTS } & ButtonHTMLAttributes<HTMLButtonElement>;

export function Button({ variant = "default", className, ...rest }: Props) {
  const cls = [
    "rounded-ctl px-4 py-2 cursor-pointer border whitespace-nowrap",
    "disabled:opacity-50 disabled:cursor-default disabled:pointer-events-none",
    VARIANTS[variant],
    className ?? "",
  ].join(" ");
  return <button type="button" {...rest} className={cls} />;
}
