package com.cfdata.app;

import android.app.Activity;
import android.content.res.Configuration;
import android.graphics.Color;
import android.os.Build;
import android.os.Bundle;
import android.view.Gravity;
import android.view.View;
import android.view.WindowInsets;
import android.view.WindowInsetsController;
import android.view.WindowManager;
import android.webkit.JavascriptInterface;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.FrameLayout;
import android.widget.TextView;

import com.cfdata.mobile.cfdata.Cfdata;

public class MainActivity extends Activity {
    private static final int LIGHT_BG = Color.rgb(0xF3, 0xF4, 0xF6);
    private static final int DARK_BG = Color.rgb(0x0F, 0x17, 0x2A);

    private FrameLayout root;
    private WebView webView;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        getWindow().addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);

        root = new FrameLayout(this);
        applyInsets(root);
        setContentView(root);

        webView = new WebView(this);
        WebSettings settings = webView.getSettings();
        settings.setJavaScriptEnabled(true);
        settings.setDomStorageEnabled(true);
        settings.setDatabaseEnabled(true);
        settings.setAllowFileAccess(false);
        settings.setAllowContentAccess(false);
        webView.setWebViewClient(new WebViewClient() {
            @Override
            public void onPageFinished(WebView view, String url) {
                // Android 上“关闭程序”没有意义，隐藏掉，用返回键退出
                view.evaluateJavascript(
                        "(function(){var b=document.getElementById('btnShutdown');if(b){b.style.display='none';}})();",
                        null);
            }
        });
        webView.addJavascriptInterface(new ThemeBridge(), "CFScanNative");
        root.addView(webView, new FrameLayout.LayoutParams(
                FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT));
        applyPageTheme(isSystemDarkMode());

        final TextView loading = new TextView(this);
        loading.setText("正在启动本地服务...");
        loading.setTextSize(16);
        loading.setGravity(Gravity.CENTER);
        root.addView(loading, new FrameLayout.LayoutParams(
                FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT));

        final String baseDir = getFilesDir().getAbsolutePath();
        new Thread(() -> {
            try {
                final String url = Cfdata.start(baseDir);
                runOnUiThread(() -> {
                    root.removeView(loading);
                    webView.loadUrl(url);
                });
            } catch (final Exception e) {
                runOnUiThread(() -> loading.setText("启动失败: " + e.getMessage()));
            }
        }, "cfdata-start").start();
    }

    private boolean isSystemDarkMode() {
        return (getResources().getConfiguration().uiMode & Configuration.UI_MODE_NIGHT_MASK)
                == Configuration.UI_MODE_NIGHT_YES;
    }

    @SuppressWarnings("deprecation")
    private void applyPageTheme(boolean dark) {
        int bg = dark ? DARK_BG : LIGHT_BG;
        if (root != null) root.setBackgroundColor(bg);
        if (webView != null) webView.setBackgroundColor(bg);

        if (Build.VERSION.SDK_INT < 35) {
            getWindow().setStatusBarColor(bg);
            getWindow().setNavigationBarColor(bg);
        }

        int lightFlags = View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR | View.SYSTEM_UI_FLAG_LIGHT_NAVIGATION_BAR;
        if (Build.VERSION.SDK_INT >= 30) {
            WindowInsetsController controller = getWindow().getInsetsController();
            if (controller != null) {
                int mask = WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS
                        | WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS;
                controller.setSystemBarsAppearance(dark ? 0 : mask, mask);
            }
        } else {
            int vis = getWindow().getDecorView().getSystemUiVisibility();
            if (dark) vis &= ~lightFlags;
            else vis |= lightFlags;
            getWindow().getDecorView().setSystemUiVisibility(vis);
        }
    }

    private class ThemeBridge {
        @JavascriptInterface
        public boolean isSystemDark() {
            return isSystemDarkMode();
        }

        @JavascriptInterface
        public void setPageDark(final boolean dark) {
            runOnUiThread(() -> applyPageTheme(dark));
        }
    }

    private void applyInsets(View view) {
        if (Build.VERSION.SDK_INT >= 30) {
            view.setOnApplyWindowInsetsListener((v, insets) -> {
                android.graphics.Insets bars = insets.getInsets(
                        WindowInsets.Type.systemBars() | WindowInsets.Type.ime());
                v.setPadding(bars.left, bars.top, bars.right, bars.bottom);
                return insets;
            });
        } else {
            view.setFitsSystemWindows(true);
        }
    }

    @Override
    public void onConfigurationChanged(Configuration newConfig) {
        super.onConfigurationChanged(newConfig);
        if (webView != null) {
            webView.evaluateJavascript(
                    "window.CFScanSystemThemeChanged && window.CFScanSystemThemeChanged();", null);
        }
    }

    @Override
    protected void onResume() {
        super.onResume();
        if (webView != null) {
            webView.evaluateJavascript(
                    "window.CFScanSystemThemeChanged && window.CFScanSystemThemeChanged();", null);
        }
    }

    @Override
    public void onBackPressed() {
        if (webView != null && webView.canGoBack()) {
            webView.goBack();
            return;
        }
        super.onBackPressed();
    }

    @Override
    protected void onDestroy() {
        if (webView != null) {
            root.removeView(webView);
            webView.loadUrl("about:blank");
            webView.destroy();
            webView = null;
        }
        Cfdata.stop();
        super.onDestroy();
    }
}
