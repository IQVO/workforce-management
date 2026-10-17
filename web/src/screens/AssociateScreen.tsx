import { useRef, useState, type FormEvent } from "react";
import { Card } from "@warehouse/ui-kit";
import { WORKFORCE_API_BASE } from "../config";
import { apiPost, ApiError, newIdempotencyKey } from "../api";
import { assignLaborUrl, certifyAssociateUrl } from "../apiUrls";
import type { AssignmentResponse } from "../types";

/**
 * Supervisor console for one associate's labor record: the two SUPERVISOR
 * write actions this context owns --
 *
 *  - Certifications: POST /associates/{id}/certifications (204, set
 *    semantics -- re-adding a held certification is a no-op).
 *  - Path assignment: POST /associates/{id}/assignments, which closes any
 *    active assignment first (exactly one ACTIVE assignment per associate,
 *    enforced by construction).
 *
 * Every POST carries an Idempotency-Key (ADR-0027): minted per logical
 * submission, kept while that submission's outcome is unknown (so a retry
 * after a network failure cannot double-apply), dropped once the service
 * answers or the form is edited -- the same UX order-management's screen
 * established for the fleet.
 *
 * The service exposes NO read endpoint for an associate's current state,
 * so the certification list and assignment log below are tracked from
 * this console's own actions for the session -- labeled as such, never
 * presented as the server's authoritative view.
 */
export function AssociateScreen() {
  const [associateInput, setAssociateInput] = useState("");
  const [associateId, setAssociateId] = useState<string | null>(null);

  // --- Certifications ---
  const [certInput, setCertInput] = useState("");
  const [certs, setCerts] = useState<string[]>([]);
  const [certifying, setCertifying] = useState(false);
  const [certError, setCertError] = useState<string | null>(null);
  const [lastCert, setLastCert] = useState<string | null>(null);
  const certSubmissionKey = useRef<string | null>(null);

  // --- Path assignment ---
  const [pathInput, setPathInput] = useState("");
  const [assignment, setAssignment] = useState<AssignmentResponse | null>(null);
  const [assignLog, setAssignLog] = useState<string[]>([]);
  const [assigning, setAssigning] = useState(false);
  const [assignError, setAssignError] = useState<string | null>(null);
  const assignSubmissionKey = useRef<string | null>(null);

  function onSelectAssociate(e: FormEvent) {
    e.preventDefault();
    const id = associateInput.trim();
    if (!id) return;
    setAssociateId(id);
    // Session-tracked state belongs to the previously selected associate.
    setCerts([]);
    setLastCert(null);
    setCertError(null);
    setCertInput("");
    certSubmissionKey.current = null;
    setAssignment(null);
    setAssignLog([]);
    setAssignError(null);
    setPathInput("");
    assignSubmissionKey.current = null;
  }

  async function onCertify(e: FormEvent) {
    e.preventDefault();
    if (!associateId) return;
    const certification = certInput.trim();
    if (!certification) return;
    setCertError(null);
    setCertifying(true);
    const idempotencyKey = (certSubmissionKey.current ??= newIdempotencyKey());
    try {
      await apiPost(certifyAssociateUrl(WORKFORCE_API_BASE, associateId), { certification }, idempotencyKey);
      // The service answered: that outcome is final (and cached under this
      // key), so a next click is a new submission with a new key.
      certSubmissionKey.current = null;
      setCertInput("");
      setLastCert(certification);
      // Set semantics server-side: a repeat add is a no-op there, so the
      // session list dedupes the same way.
      setCerts((prev) => (prev.includes(certification) ? prev : [...prev, certification]));
    } catch (err) {
      // An ApiError means the service answered (that answer is replayed
      // for this key on retry) -- the next attempt is a new submission.
      // A network failure leaves the outcome unknown, so the key stays
      // for the retry.
      if (err instanceof ApiError) certSubmissionKey.current = null;
      setCertError(err instanceof ApiError ? err.message : "Failed to add certification.");
    } finally {
      setCertifying(false);
    }
  }

  async function onAssign(e: FormEvent) {
    e.preventDefault();
    if (!associateId) return;
    const pathId = pathInput.trim();
    if (!pathId) return;
    setAssignError(null);
    setAssigning(true);
    const idempotencyKey = (assignSubmissionKey.current ??= newIdempotencyKey());
    try {
      const response = await apiPost<AssignmentResponse>(
        assignLaborUrl(WORKFORCE_API_BASE, associateId),
        { pathId },
        idempotencyKey,
      );
      assignSubmissionKey.current = null;
      setPathInput("");
      setAssignment(response);
      setAssignLog((prev) => [pathId, ...prev]);
    } catch (err) {
      if (err instanceof ApiError) assignSubmissionKey.current = null;
      setAssignError(err instanceof ApiError ? err.message : "Failed to assign associate.");
    } finally {
      setAssigning(false);
    }
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: "var(--wh-space-5)" }}>
      <div>
        <h1 style={{ fontSize: "var(--wh-font-size-2xl)", margin: 0 }}>Associates</h1>
        <p style={{ color: "var(--wh-color-text-muted)", marginTop: 4 }}>
          workforce-management · supervisor console -- certifications and path assignments
        </p>
      </div>

      <form onSubmit={onSelectAssociate} style={{ display: "flex", gap: "var(--wh-space-2)", flexWrap: "wrap" }}>
        <input
          value={associateInput}
          onChange={(e) => setAssociateInput(e.target.value)}
          placeholder="Associate ID"
          aria-label="Associate ID"
          style={inputStyle({ flex: 1, minWidth: 200 })}
        />
        <button type="submit" style={buttonStyle}>
          Select associate
        </button>
      </form>

      {!associateId && (
        <Card>
          <div style={{ color: "var(--wh-color-text-muted)" }}>
            Enter an associate ID to manage its certifications and path assignment.
          </div>
        </Card>
      )}

      {associateId && (
        <>
          <Card title={`Certifications · ${associateId}`}>
            <div style={{ display: "flex", flexDirection: "column", gap: "var(--wh-space-4)" }}>
              <div
                style={{
                  color: "var(--wh-color-text-muted)",
                  fontSize: "var(--wh-font-size-sm)",
                }}
              >
                Added through this console, tracked for this session -- the service exposes
                no associate read endpoint yet. By convention a path requires the
                certification of the same name (path “pack” requires certification “pack”).
              </div>
              {certs.length > 0 ? (
                <div style={{ display: "flex", gap: "var(--wh-space-2)", flexWrap: "wrap" }}>
                  {certs.map((cert) => (
                    <span key={cert} style={tagStyle}>
                      {cert}
                    </span>
                  ))}
                </div>
              ) : (
                <div style={{ color: "var(--wh-color-text-muted)", fontSize: "var(--wh-font-size-sm)" }}>
                  No certifications recorded this session.
                </div>
              )}
              <form onSubmit={onCertify} style={{ display: "flex", gap: "var(--wh-space-2)", flexWrap: "wrap" }}>
                <input
                  value={certInput}
                  onChange={(e) => {
                    certSubmissionKey.current = null;
                    setCertInput(e.target.value);
                  }}
                  placeholder="Certification (e.g. hazmat)"
                  aria-label="Certification name"
                  style={inputStyle({ flex: 1, minWidth: 200 })}
                />
                <button type="submit" disabled={certifying || !certInput.trim()} style={buttonStyle}>
                  {certifying ? "Adding…" : "Add certification"}
                </button>
              </form>
              {lastCert && !certError && (
                <div style={{ color: "var(--wh-color-status-success)", fontSize: "var(--wh-font-size-sm)" }}>
                  Added “{lastCert}”.
                </div>
              )}
              {certError && (
                <div style={{ color: "var(--wh-color-status-danger)", fontSize: "var(--wh-font-size-sm)" }}>
                  {certError}
                </div>
              )}
            </div>
          </Card>

          <Card title={`Path assignment · ${associateId}`}>
            <div style={{ display: "flex", flexDirection: "column", gap: "var(--wh-space-4)" }}>
              <div
                style={{
                  color: "var(--wh-color-text-muted)",
                  fontSize: "var(--wh-font-size-sm)",
                }}
              >
                Assigning to a path closes any active assignment first (exactly one active
                assignment per associate). The associate must hold the path’s required
                certification.
              </div>
              {assignment && (
                <div style={{ fontSize: "var(--wh-font-size-sm)" }}>
                  {assignment.active && assignment.activePathId ? (
                    <>
                      Active path:{" "}
                      <span style={{ fontFamily: "var(--wh-font-mono)", fontWeight: 600 }}>
                        {assignment.activePathId}
                      </span>
                    </>
                  ) : (
                    "No active assignment."
                  )}
                </div>
              )}
              {assignLog.length > 0 && (
                <div style={{ display: "flex", gap: "var(--wh-space-2)", flexWrap: "wrap", alignItems: "center" }}>
                  <span
                    style={{
                      color: "var(--wh-color-text-muted)",
                      fontSize: "var(--wh-font-size-sm)",
                    }}
                  >
                    Assigned this session:
                  </span>
                  {assignLog.map((pathId, i) => (
                    <span key={`${pathId}-${i}`} style={tagStyle}>
                      {pathId}
                    </span>
                  ))}
                </div>
              )}
              <form onSubmit={onAssign} style={{ display: "flex", gap: "var(--wh-space-2)", flexWrap: "wrap" }}>
                <input
                  value={pathInput}
                  onChange={(e) => {
                    assignSubmissionKey.current = null;
                    setPathInput(e.target.value);
                  }}
                  placeholder="Path ID (e.g. pack)"
                  aria-label="Path ID"
                  style={inputStyle({ flex: 1, minWidth: 200 })}
                />
                <button type="submit" disabled={assigning || !pathInput.trim()} style={buttonStyle}>
                  {assigning ? "Assigning…" : "Assign to path"}
                </button>
              </form>
              {assignError && (
                <div style={{ color: "var(--wh-color-status-danger)", fontSize: "var(--wh-font-size-sm)" }}>
                  {assignError}
                </div>
              )}
            </div>
          </Card>
        </>
      )}
    </div>
  );
}

const tagStyle = {
  display: "inline-flex",
  padding: "4px 10px",
  borderRadius: "var(--wh-radius-pill)",
  background: "var(--wh-color-bg-sunken)",
  border: "1px solid var(--wh-color-border)",
  color: "var(--wh-color-text)",
  fontFamily: "var(--wh-font-mono)",
  fontSize: "var(--wh-font-size-sm)",
  lineHeight: 1.4,
} as const;

function inputStyle(extra: Record<string, string | number>) {
  return {
    padding: "10px 12px",
    borderRadius: "var(--wh-radius-md)",
    border: "1px solid var(--wh-color-border)",
    background: "var(--wh-color-bg-sunken)",
    color: "var(--wh-color-text)",
    fontFamily: "var(--wh-font-mono)",
    fontSize: "var(--wh-font-size-sm)",
    ...extra,
  } as const;
}

const buttonStyle = {
  padding: "10px 18px",
  borderRadius: "var(--wh-radius-md)",
  border: "none",
  background: "var(--wh-color-accent)",
  color: "#fff",
  fontWeight: 600,
  fontSize: "var(--wh-font-size-sm)",
  cursor: "pointer",
} as const;
