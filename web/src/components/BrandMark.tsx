interface BrandMarkProps {
  className?: string;
}

// BrandMark is the branchDAM logo: a geometric lowercase "b" where the stem
// is the immutable master, the bowl's counter is a graph node, and the
// horizontal edge exits to a satellite derivative node -- the same
// left-to-right lineage direction AssetGraphCanvas.tsx lays out for the
// asset graph itself. Single flat currentColor shape, no gradients, so it
// inherits whatever text color the caller sets (see App.tsx sidebar for the
// canonical usage).
export default function BrandMark({ className }: BrandMarkProps) {
  return (
    <svg viewBox="0 0 64 64" fill="none" className={className} aria-hidden="true">
      <rect x="6" y="5" width="9" height="54" rx="4.5" fill="currentColor" />
      <circle cx="28" cy="40" r="14.5" stroke="currentColor" strokeWidth="7.5" />
      <path d="M43 40 H48" stroke="currentColor" strokeWidth="6" strokeLinecap="round" />
      <circle cx="54" cy="40" r="6" fill="currentColor" />
    </svg>
  );
}
