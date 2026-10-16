declare module "@novnc/novnc" {
  const RFB: new (target: HTMLElement, url: string, options?: { credentials?: { password?: string }; wsProtocols?: string[] }) => unknown;
  export default RFB;
}
