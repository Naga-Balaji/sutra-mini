export const metadata = { title: "Sutra Mini" };

export default function RootLayout({ children }) {
  return (
    <html lang="en">
      <body style={{ fontFamily: "system-ui, sans-serif", margin: "24px", color: "#222" }}>
        {children}
      </body>
    </html>
  );
}
