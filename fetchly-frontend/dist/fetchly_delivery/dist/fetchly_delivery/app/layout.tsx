import { ReactNode } from "react";
import DashboardLayout from "./components/DashboardLayout";
import "./globals.css";

// Define props for the root layout
interface RootLayoutProps {
  children: ReactNode;
}

export default function RootLayout({ children }: RootLayoutProps) {
  // get children properties
  console.log("RootLayout children props: ", children);

  return (
    <html lang="en">
      <body>
        <DashboardLayout>
          {children}
        </DashboardLayout>
      </body>
    </html>
  );
}