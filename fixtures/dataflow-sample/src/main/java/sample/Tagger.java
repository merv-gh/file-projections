package sample;

public class Tagger {
    public String tag(String coupon) {
        String base = coupon.toUpperCase();
        String prefix = "T";
        String mid = prefix;
        String body = prefix + "-" + mid;
        String result = body;
        return result;
    }
}
