import { useCallback, useEffect, useState } from "react";
import { Outlet, useNavigate } from "react-router-dom";
import Sidebar from "./Sidebar";
import CommandPalette from "./CommandPalette";
import TopBar from "./TopBar";

const GO_ROUTES: Record<string, string> = {
  d: "/",
  r: "/rules",
  t: "/traces",
  l: "/logs",
  m: "/metrics",
  f: "/fleet",
};

function isTypingTarget(target: EventTarget | null): boolean {
  const element = target as HTMLElement | null;
  if (!element) return false;
  const tag = element.tagName;
  return (
    tag === "INPUT" ||
    tag === "TEXTAREA" ||
    tag === "SELECT" ||
    element.isContentEditable
  );
}

export default function AppLayout() {
  const navigate = useNavigate();
  const [paletteOpen, setPaletteOpen] = useState(false);

  const closePalette = useCallback(() => setPaletteOpen(false), []);

  useEffect(() => {
    let lastG = 0;

    const onKeyDown = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setPaletteOpen((open) => !open);
        return;
      }

      if (event.key === "Escape") {
        setPaletteOpen(false);
        return;
      }

      if (isTypingTarget(event.target)) return;

      if (event.key === "/") {
        event.preventDefault();
        setPaletteOpen(true);
        return;
      }

      if (event.key.toLowerCase() === "g") {
        lastG = Date.now();
        return;
      }

      if (Date.now() - lastG < 1200) {
        const route = GO_ROUTES[event.key.toLowerCase()];
        if (route) {
          event.preventDefault();
          lastG = 0;
          navigate(route);
        }
      }
    };

    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [navigate]);

  return (
    <div className="app">
      <Sidebar onOpenPalette={() => setPaletteOpen(true)} />
      <main className="main">
        <TopBar />
        <div className="main-view">
          <Outlet />
        </div>
      </main>
      <CommandPalette open={paletteOpen} onClose={closePalette} />
    </div>
  );
}
