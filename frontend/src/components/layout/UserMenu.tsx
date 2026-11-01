import { useEffect, useRef, useState } from "react";
import { ChevronDown, LogOut, UserRound } from "lucide-react";
import { api } from "../../api/client";
import ThemePicker from "./ThemePicker";

export default function UserMenu() {
  const [open, setOpen] = useState(false);
  const [user, setUser] = useState("");
  const ref = useRef<HTMLDivElement>(null);

  useEffect(() => {
    let active = true;
    api
      .getUser()
      .then((identity) => {
        if (!active) return;
        setUser(
          identity.display ||
            identity.email ||
            identity.subject ||
            identity.client_id ||
            ""
        );
      })
      .catch(() => {});
    return () => {
      active = false;
    };
  }, []);

  useEffect(() => {
    if (!open) return;
    const onPointer = (event: MouseEvent) => {
      if (!ref.current?.contains(event.target as Node)) setOpen(false);
    };
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onPointer);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onPointer);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  return (
    <div className="relative" ref={ref}>
      <button
        type="button"
        className="s-icon-btn"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label="Open user menu"
        onClick={() => setOpen((value) => !value)}
      >
        <UserRound size={16} />
        <ChevronDown size={12} />
      </button>
      {open && (
        <div className="s-menu absolute right-0 top-full z-50 mt-2" role="menu">
          <div className="px-2 py-2 border-b border-dark-600">
            <div className="text-xs font-semibold text-gray-200">
              {user || "Signed in"}
            </div>
            <div className="text-[11px] text-gray-500">Strata platform</div>
          </div>
          <div className="px-2 py-2 border-b border-dark-600">
            <ThemePicker />
          </div>
          <a className="s-menu__item" href="/auth/logout">
            <LogOut size={14} />
            Sign out
          </a>
        </div>
      )}
    </div>
  );
}
