// Placeholder rows for the field column while extraction is in flight.

export function SkeletonRow() {
  return (
    <div className="mb-4">
      <div className="h-3 w-[30%] mb-1.5 rounded-[4px] bg-line animate-pulse-skel" />
      <div className="h-9 rounded-ctl bg-line animate-pulse-skel" />
    </div>
  );
}
