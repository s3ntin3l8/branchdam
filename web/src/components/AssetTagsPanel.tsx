import { useState, type FormEvent } from "react";
import { useAddAssetTag, useAssetAnnotations, useAssetTags, useDeleteAssetTag } from "../hooks/queries";
import type { AssetAnnotation, AssetTag } from "../api/types";

function SourceBadge({ source }: { source: AssetTag["source"] | AssetAnnotation["source"] }) {
  if (source !== "assistant") return null;
  return (
    <span className="rounded border border-violet-800 bg-violet-950 px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-violet-200">
      AI
    </span>
  );
}

export default function AssetTagsPanel({ assetId }: { assetId: number }) {
  const { data: tags, isLoading: tagsLoading, isError: tagsError } = useAssetTags(assetId);
  const { data: notes, isLoading: notesLoading, isError: notesError } = useAssetAnnotations(assetId);
  const addTag = useAddAssetTag(assetId);
  const removeTag = useDeleteAssetTag(assetId);
  const [draft, setDraft] = useState("");

  const descriptions = (notes?.annotations ?? []).filter((row) => row.kind === "description");

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const tag = draft.trim();
    if (!tag) return;
    addTag.mutate(tag, { onSuccess: () => setDraft("") });
  };

  return (
    <section className="space-y-4">
      <div>
        <h2 className="mb-2 text-sm font-medium text-neutral-400">Description</h2>
        {notesLoading ? (
          <p className="text-sm text-neutral-500">Loading description…</p>
        ) : notesError ? (
          <p className="text-sm text-red-400">Failed to load the description.</p>
        ) : descriptions.length > 0 ? (
          <ul className="space-y-2" aria-label="Asset descriptions">
            {descriptions.map((row) => (
              <li key={row.id} className="flex flex-wrap items-center gap-2 text-sm text-neutral-200">
                <span>{row.text}</span>
                <SourceBadge source={row.source} />
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-sm text-neutral-500">No description.</p>
        )}
      </div>
      <div>
        <h2 className="mb-2 text-sm font-medium text-neutral-400">Tags</h2>
        {tagsLoading ? (
          <p className="text-sm text-neutral-500">Loading tags…</p>
        ) : tagsError ? (
          <p className="text-sm text-red-400">Failed to load tags.</p>
        ) : (
          <ul className="mb-3 flex flex-wrap gap-2" aria-label="Asset tags">
            {(tags?.tags ?? []).map((row) => (
              <li key={row.id} className="flex items-center gap-1 rounded-full border border-neutral-700 bg-neutral-900 px-2 py-1 text-xs text-neutral-200">
                <span>{row.tag}</span>
                <SourceBadge source={row.source} />
                <button
                  type="button"
                  aria-label={`Remove tag ${row.tag}`}
                  disabled={removeTag.isPending}
                  onClick={() => removeTag.mutate(row.tag)}
                  className="text-neutral-500 hover:text-neutral-200 disabled:opacity-50"
                >
                  ×
                </button>
              </li>
            ))}
            {(tags?.tags ?? []).length === 0 && <li className="text-sm text-neutral-500">No tags.</li>}
          </ul>
        )}
        <form onSubmit={submit} className="flex gap-2">
          <label htmlFor="add-asset-tag" className="sr-only">
            Add tag
          </label>
          <input
            id="add-asset-tag"
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            placeholder="Add a tag"
            className="w-48 rounded border border-neutral-700 bg-neutral-800 px-2 py-1 text-sm text-neutral-200"
          />
          <button
            type="submit"
            disabled={addTag.isPending || draft.trim() === ""}
            className="rounded border border-neutral-700 bg-neutral-800 px-2 py-1 text-xs text-neutral-200 hover:bg-neutral-700 disabled:opacity-50"
          >
            Add tag
          </button>
        </form>
        {addTag.isError && <p className="mt-1 text-xs text-red-400">Could not add that tag.</p>}
        {removeTag.isError && <p className="mt-1 text-xs text-red-400">Could not remove that tag.</p>}
      </div>
    </section>
  );
}
