declare module "@novnc/novnc" {
  const RFB: new (target: HTMLElement, url: string, options?: { credentials?: { password?: string } }) => unknown;
  export default RFB;
}
