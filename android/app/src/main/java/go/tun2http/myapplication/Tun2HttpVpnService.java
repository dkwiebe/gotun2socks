package go.tun2http.myapplication;

import android.content.Context;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.net.VpnService;
import android.os.Build;
import android.os.IBinder;
import android.os.ParcelFileDescriptor;
import android.text.TextUtils;
import android.util.Base64;

import gotun2socks.Gotun2socks;


public class Tun2HttpVpnService extends VpnService {
    private static final int PROXY_TYPE_SOCKS = 1;
    private static final int PROXY_TYPE_HTTP = 2;
    private static final String ACTION_START = "start";
    private static final String ACTION_STOP = "stop";
    private static final String EXTRA_APP_PACKAGE_NAME = "app_package_name";

    private ParcelFileDescriptor parcelFileDescriptor = null;

    private String appPackageName = getPackageName();

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }


    public boolean isRunning() {
        return parcelFileDescriptor != null;
    }

    private void start() {
        if (parcelFileDescriptor == null) {
            setupProxyServers();
            Builder builder = setupBuilder();
            parcelFileDescriptor = builder.establish();
            Gotun2socks.run(parcelFileDescriptor.getFd());
        }
    }

    private void stop() {
        try {
            if (parcelFileDescriptor != null) {
                Gotun2socks.stop();
                parcelFileDescriptor.close();
                parcelFileDescriptor = null;
            }
        } catch (Throwable ex) {
        }
        stopForeground(true);
    }


    @Override
    public void onRevoke() {
        stop();
        parcelFileDescriptor = null;

        super.onRevoke();
    }


    private Builder setupBuilder() {
        // Build VPN service
        Builder builder = new Builder();
        builder.setSession(getString(R.string.app_name));

        // VPN address
        builder.addAddress("10.0.0.2", 32);
        builder.addRoute("0.0.0.0", 0);
        builder.addRoute("0:0:0:0:0:0:0:0", 0);

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.LOLLIPOP) {
            try {
                builder.addDisallowedApplication(appPackageName);
            } catch (PackageManager.NameNotFoundException e) {
                e.printStackTrace();
            }
        }

        builder.setMtu(15000);

        return builder;
    }

    private void setupProxyServers() {
        String header = Base64.encodeToString(("test@test.com" + ":" + "1").getBytes(), Base64.NO_WRAP);
        Gotun2socks.setDefaultProxy("123.123.123.123:8099", PROXY_TYPE_HTTP, header, "", "");

        Gotun2socks.addProxyServer(10062, "123.123.123.123:1009", PROXY_TYPE_SOCKS, "", "login", "password");
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        // Handle service restart
        if (intent == null) {
            return START_STICKY;
        }

        String mainPackageName = intent.getStringExtra(EXTRA_APP_PACKAGE_NAME);
        if(!TextUtils.isEmpty(mainPackageName)) {
            appPackageName = mainPackageName;
        }

        if (ACTION_START.equals(intent.getAction())) {
            start();
        }
        if (ACTION_STOP.equals(intent.getAction())) {
            stop();
        }
        return START_STICKY;
    }


    @Override
    public void onDestroy() {
        stop();
        super.onDestroy();
    }

    public static void start(Context context) {
        Intent intent = new Intent(context, Tun2HttpVpnService.class);
        intent.setAction(ACTION_START);
        intent.putExtra(EXTRA_APP_PACKAGE_NAME, context.getPackageName());

        context.startService(intent);
    }


    public static void stop(Context context) {
        Intent intent = new Intent(context, Tun2HttpVpnService.class);
        intent.setAction(ACTION_STOP);
        context.startService(intent);
    }
}