import { HashRouter, Routes, Route, Navigate } from "react-router-dom";
import AppLayout from "./components/layout/AppLayout";
import { ConfigProvider } from "./context/ConfigContext";
import DashboardPage from "./pages/DashboardPage";
import DashboardsPage from "./pages/DashboardsPage";
import ServicesPage from "./pages/ServicesPage";
import ProbesPage from "./pages/ProbesPage";
import FederationCatalogPage from "./pages/FederationCatalogPage";
import TracesPage from "./pages/TracesPage";
import LogsPage from "./pages/LogsPage";
import MetricsPage from "./pages/MetricsPage";
import FleetDashboard from "./pages/FleetDashboard";
import RecordingRulesPage from "./pages/RecordingRulesPage";
import AlertsPage from "./pages/AlertsPage";
import PartitionViewPage from "./pages/PartitionViewPage";
import PartitionAssetsPage from "./pages/PartitionAssetsPage";

export default function App() {
  return (
    <ConfigProvider>
      <HashRouter>
        <Routes>
          <Route element={<AppLayout />}>
            <Route path="/" element={<DashboardPage />} />
            <Route path="/dashboards" element={<DashboardsPage />} />
            <Route path="/dashboards/:id" element={<DashboardsPage />} />
            <Route path="/services" element={<ServicesPage />} />
            <Route path="/services/:name" element={<ServicesPage />} />
            <Route path="/federation" element={<FederationCatalogPage />} />
            <Route path="/probes" element={<ProbesPage />} />
            <Route path="/rules" element={<RecordingRulesPage />} />
            <Route path="/alerts/*" element={<AlertsPage />} />
            <Route path="/traces" element={<TracesPage />} />
            <Route path="/logs" element={<LogsPage />} />
            <Route path="/metrics" element={<MetricsPage />} />
            <Route path="/fleet" element={<FleetDashboard />} />
            <Route path="/partition/:name" element={<PartitionViewPage />} />
            <Route path="/partition/:name/assets" element={<PartitionAssetsPage />} />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Route>
        </Routes>
      </HashRouter>
    </ConfigProvider>
  );
}
