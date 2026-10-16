import { createContext, useContext } from "react";

// FieldRow renders a field's visible label; the control inside it is a
// separate component (TextField, NumberField, ...). FieldRow publishes the
// ids it generated here so the control can take the id the <label htmlFor>
// points at and be described by the row's doc text -- without every caller
// threading ids through by hand.
export interface FieldIds {
  controlId: string;
  docId?: string;
}

export const FieldIdsContext = createContext<FieldIds | null>(null);

export function useFieldIds(): FieldIds | null {
  return useContext(FieldIdsContext);
}
