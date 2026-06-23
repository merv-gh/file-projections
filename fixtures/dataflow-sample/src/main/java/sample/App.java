package sample;

public class App {
    private final Tagger tagger = new Tagger();
    private final Pricer pricer = new Pricer();

    public String summary(String coupon, int amount) {
        return tagger.tag(coupon) + "/" + pricer.price(amount);
    }
}
