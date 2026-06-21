# Data Diode Background Research

> A living document. The deep-research report (run separately) will populate the citations and fact-check the claims below.

## 1. Concept

A **data diode** enforces *one-way* data flow between two networks. The asymmetry is what makes it useful: data crosses the boundary, but no signal — not even a TCP ACK — returns. This eliminates an entire class of attack: there is no path along which a compromised receiver can reach back to the source.

The canonical mental model is the electronic diode: current flows in one direction, blocked in the other. In networking terms:

- **Low side (source / "black" / "untrusted-out")** — the network you are willing to *send from*.
- **High side (destination / "red" / "trusted-in")** — the network you are willing to *receive into*.

(Names reverse depending on whether you're protecting confidentiality or integrity. For OT→IT telemetry the *plant* is the high side and the *enterprise* is the low side. For classified networks the *secret* enclave is the high side. The diode mechanics are identical; only the policy labels move.)

## 2. Hardware Diodes

A hardware data diode is, in its purest form, an optical link with the **receiver photodiode physically removed** on one side and the **transmitter LED removed** on the other. The fiber carries light in only one direction because there is no transmitter at the other end and no receiver on this end. No firmware, no software, no configuration can defeat this — the silicon to send is not present.

Real products sit on top of this primitive with:
- A **transmit proxy** on the low side that terminates a normal bidirectional protocol (TCP, OPC, SMB, etc.).
- A **one-way wire** between the two appliances.
- A **receive proxy** on the high side that reconstructs the protocol and delivers to the destination.

Notable vendors (illustrative, to be verified by the deep-research report):
- **Owl Cyber Defense** (formerly Owl Computing) — DualDiode line.
- **Waterfall Security Solutions** — Unidirectional Security Gateways, common in power/oil&gas.
- **Fox-IT (NCC Group)** — DataDiode, NATO-certified variants.
- **Advenica** — SecuriCDS.
- **BAE Systems** — DataDiode.
- **Siemens RUGGEDCOM** — industrial variants.

## 3. Software Diodes

A *software* diode cannot match the physical guarantee — a kernel bug, NIC firmware bug, or misconfigured iptables rule could in principle allow reverse traffic. But for many real threat models, a software diode plus deliberate hardening (separate NICs, host firewall blocking all egress on the receiver, no listening sockets on the sender's receive side, minimal OS) is *sufficient* and *vastly cheaper* than a hardware appliance.

Software diodes universally use **UDP** for transport, because TCP requires a return path (ACKs, window updates, FIN). The challenges that follow are:

- **No retransmission** — packet loss is permanent. Mitigated with **Forward Error Correction (FEC)** (Reed-Solomon, Raptor codes) and/or **redundant transmission** (send each packet N times).
- **No flow control** — sender must respect a configured rate or risk overwhelming the receiver.
- **No handshake** — receiver must tolerate restart of either side at any moment.
- **No integrity from TCP** — every message carries its own hash (SHA-256 typical) and optionally a signature.

Existing open / semi-open work to study:
- `udp-broadcast-relay-redux` — not a diode but the same one-way-UDP plumbing.
- `usbdatadiode` — hobbyist hardware diode using two USB-to-serial adapters with TX/RX wires cut.
- Various academic prototypes (search: *"software-based data diode"*, *"one-way file transfer protocol"*).

## 4. Protocols Commonly Carried

Operators rarely speak "raw diode." They want their existing apps to *just work*. So a real diode product ships protocol proxies for:

- **Syslog** (UDP already → easiest)
- **File transfer** (drop a file on low side, appears on high side)
- **SMTP** (mail relay across the boundary)
- **HTTP(S)** GET-only mirroring
- **TCP stream** (generic, ordered byte stream — sender terminates, diode carries, receiver re-emits)
- **OPC-UA / Modbus** (industrial telemetry — single most common real-world use case)
- **MQTT** (IoT telemetry)
- **NTP** (one-way time distribution)
- **Database replication** (CDC events as messages)
- **Video / streaming** (RTP-style)

Our plugin model exists exactly to let these be added incrementally.

## 5. Security Guarantees and Their Limits

A software diode buys you:
- **No application-layer reverse path** — by construction, no plugin can open a return socket.
- **Strong integrity per message** — hashes and optional signatures detect tampering or corruption.
- **Small, auditable codebase** — orders of magnitude smaller than a firewall.

A software diode does **not** buy you:
- **Physical impossibility of reverse traffic** — that requires hardware.
- **Protection against a compromised sender host** — if the low-side OS is owned, the attacker can choose what to send.
- **Confidentiality on the wire** — must be layered on (TLS-on-UDP, DTLS, application-layer encryption). Note: DTLS uses a handshake, which is bidirectional; pure one-way confidentiality requires pre-shared keys.
- **Covert channel resistance** — timing and message-rate side channels exist; mitigated by rate-shaping and constant-rate transmission.

## 6. Where Our Project Fits

This project targets the **software-diode niche**: operators who want diode *discipline* (no return path, integrity per message, plugin-based protocol coverage) on commodity hardware, without paying for an appliance, and who accept the residual risk that comes with a software-enforced boundary.

Typical fit:
- SOC pulling logs from a sensitive subnet.
- Home-lab / SMB operators isolating IoT.
- Research / academic networks.
- Pre-production / staging boundaries.
- A *defense-in-depth* layer behind a real hardware diode.

Out of scope (at least initially):
- Certified cross-domain solutions.
- Replacing a hardware diode in a classified environment.
- Multi-gigabit line-rate throughput.
