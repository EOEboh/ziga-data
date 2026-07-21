import { InputHTMLAttributes, ReactNode } from "react";

// AuthCard is the centered narrow container shared by every auth / onboarding
// screen, matching the app's calm card pattern (surface + line + rounded-card).
export function AuthCard({ title, subtitle, children }: { title: string; subtitle?: ReactNode; children?: ReactNode }) {
  return (
    <div className="min-h-dvh flex items-center justify-center p-6">
      <div className="w-full max-w-[400px]">
        <div className="text-center mb-6">
          <div className="text-lg font-semibold text-text">Ziga</div>
        </div>
        <div className="bg-surface border border-line rounded-card p-6">
          <h1 className="text-base font-semibold text-text mb-1">{title}</h1>
          {subtitle && <p className="text-sm text-text-2 mb-4">{subtitle}</p>}
          {children}
        </div>
      </div>
    </div>
  );
}

// TextField is a labeled input matching the app's control styling.
export function TextField({ label, ...rest }: { label: string } & InputHTMLAttributes<HTMLInputElement>) {
  return (
    <label className="block mb-3">
      <span className="block text-sm text-text-2 mb-1">{label}</span>
      <input
        {...rest}
        className="w-full rounded-ctl border border-line bg-bg px-3 py-2 text-text outline-none focus:border-text-2"
      />
    </label>
  );
}

// FormError renders an inline error line in the reserved red-text token.
export function FormError({ message }: { message: string | null }) {
  if (!message) return null;
  return <p className="text-sm text-red-text mb-3">{message}</p>;
}

// FormNote renders a neutral confirmation / helper line.
export function FormNote({ children }: { children: ReactNode }) {
  return <p className="text-sm text-text-2 mt-4">{children}</p>;
}
