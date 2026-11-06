/** The version of this SDK. */
export const version = '0.1.0';

/** Signal protocol revision this SDK speaks. */
export const protocolVersion = 17;

/** Client feature level, reported in ParticipantInfo so other clients can detect what this one handles. */
export const clientProtocol = 2;

// Servers read the `version` parameter as a compatibility statement, not a label: one that sees a JS
// client older than 1.6.3 does not send ReconnectResponse, so a resume could never succeed. This SDK's
// own numbering starts lower than that, so what it reports is the behaviour level it implements. Its real
// version travels separately, in the `streamkit_sdk` parameter.
export const wireCompatVersion = '2.23.0';
